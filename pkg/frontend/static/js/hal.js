// HAL frontend behaviour: toggle devices, follow live state over the websocket.
// Plain DOM APIs so that no framework has to be vendored into the binary.

(function () {
    "use strict";

    var LOG_LIMIT = 200;

    var statusEl = document.getElementById("status");
    var overviewEl = document.getElementById("overview");
    var logEl = document.getElementById("mqttLog");

    function setStatus(state, text) {
        if (!statusEl) {
            return;
        }
        statusEl.dataset.state = state;

        // role="status" announces the whole region on every mutation, and a
        // flapping connection would otherwise re-announce "Reconnecting" on each
        // backoff step. Only touch the text when it actually changes.
        var textEl = statusEl.querySelector(".status-text");
        if (textEl && textEl.textContent !== text) {
            textEl.textContent = text;
        }
    }

    // --- Device rows -------------------------------------------------------

    function rowFor(id) {
        // Device ids come from the config file, so avoid building a selector
        // out of one; querySelectorAll would throw on anything unusual.
        var rows = document.querySelectorAll(".device");
        for (var i = 0; i < rows.length; i++) {
            if (rows[i].dataset.device === id) {
                return rows[i];
            }
        }
        return null;
    }

    // paint reflects state in the row without touching the checkbox, which the
    // browser has already flipped when the change came from a click.
    function paint(row, state) {
        row.classList.toggle("is-on", state);
        row.querySelector(".device-state").textContent = state ? "On" : "Off";
        refreshCounts();
    }

    function refreshCounts() {
        var total = 0;
        var on = 0;

        document.querySelectorAll(".room").forEach(function (room) {
            var rooms = room.querySelectorAll(".device");
            var roomOn = 0;
            rooms.forEach(function (row) {
                if (row.classList.contains("is-on")) {
                    roomOn++;
                }
            });

            room.classList.toggle("has-on", roomOn > 0);
            var count = room.querySelector(".room-count");
            if (count) {
                count.textContent = roomOn + "/" + rooms.length + " on";
            }

            total += rooms.length;
            on += roomOn;
        });

        if (!overviewEl) {
            return;
        }
        // Built from nodes rather than from innerHTML. Only integers reach it
        // today, but it is the one place in this file that would interpret
        // markup, and a device name is one refactor away from arriving here.
        overviewEl.textContent = "";
        if (total === 0) {
            return;
        }
        var strong = document.createElement("b");
        strong.textContent = String(on);
        overviewEl.appendChild(strong);
        overviewEl.appendChild(document.createTextNode(" of " + total + " on"));
    }

    // announce writes into the visually hidden live region, so a state change
    // the user did not cause - another browser, the physical button, a resync -
    // is spoken rather than only shown.
    var announceEl = document.getElementById("announce");

    var announceTimer = null;

    // announce writes text into the live region. Repeats are dropped, because
    // Tasmota republishes tele/<id>/STATE on a timer and a screen reader would
    // otherwise say "Desk Lamp off" for every device every few minutes with
    // nothing having happened.
    //
    // repeat forces one through anyway, by clearing the region first: a second
    // failure that reads the same as the first is still news, and silence there
    // looks like the retry worked.
    function announce(text, repeat) {
        if (!announceEl) {
            return;
        }
        clearTimeout(announceTimer);

        if (announceEl.textContent === text) {
            if (!repeat) {
                return;
            }
            announceEl.textContent = "";
            announceTimer = setTimeout(function () {
                announceEl.textContent = text;
            }, 100);
            return;
        }
        announceEl.textContent = text;
    }

    // --- Requests ------------------------------------------------------------

    var REQUEST_TIMEOUT = 10000;

    // request bounds a call the way the server cannot. A phone that walks out of
    // WiFi mid-request leaves the socket open with nothing coming back, and
    // without this the promise never settles: the switch stays pending and that
    // device's queue stops accepting taps.
    //
    // consume reads the body, and is called inside the timeout rather than after
    // it, because fetch() resolves as soon as the headers arrive - clearing the
    // timer there would leave the body read unbounded, which is the same stall
    // one step later.
    function request(url, options, consume) {
        var abort = new AbortController();
        var timer = setTimeout(function () {
            abort.abort();
        }, REQUEST_TIMEOUT);

        options = options || {};
        options.signal = abort.signal;

        return fetch(url, options).then(function (response) {
            if (!response.ok) {
                throw new Error("HTTP " + response.status);
            }
            return consume(response);
        }).finally(function () {
            clearTimeout(timer);
        });
    }

    // An aborted request surfaces as "signal is aborted without reason", which
    // says nothing useful to someone reading the activity log.
    function describe(err) {
        return err && err.name === "AbortError" ? "timed out" : err.message;
    }

    // --- Switching ---------------------------------------------------------

    // One token per device while a PUT is in flight. Anything that learns the real
    // state in the meantime - a websocket event, a newer click - drops the token,
    // which is what stops a late failure from reverting to a state that is no
    // longer the one we started from.
    var inFlight = Object.create(null);

    // Per device, a counter of everything that has changed what the page believes
    // since it loaded - an authoritative event, or the user's own tap. A resync
    // compares it against the value captured before its request went out, so a
    // snapshot that was already stale on arrival cannot overwrite a newer fact.
    // It is not monotonic: a burst of taps that all failed winds its own bumps
    // back off, because a tap that achieved nothing taught the page nothing. That
    // is why applyStates compares for inequality rather than for a newer value.
    var epoch = Object.create(null);

    // One promise chain per device, so two taps cannot race each other to the
    // broker. Without this the second PUT can overtake the first and leave the
    // lamp in the state the user did not ask for, with the page none the wiser.
    var chain = Object.create(null);

    // How many requests are outstanding per device, so the pending state clears
    // when the last one settles rather than when the newest token happens to win.
    var queued = Object.create(null);

    // Per device, what the current run of overlapping taps started from. See the
    // change handler.
    var burst = Object.create(null);

    // Per device, a count of authoritative events seen. A request compares it
    // against the value when it was sent: an event that arrived in between is
    // newer than the request, and must not be overwritten by the request's own
    // result. Without this, suppressing the mid-request repaint - which is what
    // stops stale telemetry reverting an optimistic switch - meant a genuinely
    // newer event was dropped instead, and the row kept the value the user
    // asked for while the lamp sat in the state someone else had just set.
    var eventSeq = Object.create(null);

    // Per device, the timer clearing its failure cue, so a second failure
    // restarts the cue instead of inheriting the first one's deadline.
    var failedTimers = Object.create(null);

    // Per device, the pending reconciliation described in the change handler.
    var reconcileTimers = Object.create(null);

    // How long to give a device to echo a command before asking the server who
    // was right.
    //
    // Measured against a Tasmota echo: at 120ms and at 500ms the reconcile
    // lands after the echo and only confirms what the event already painted.
    // Above this value the old bounce returns, bounded by (echo - this). What
    // it costs is the other case: a switch genuinely contradicted by someone
    // else keeps showing what the user asked for for this long before it is
    // corrected. Raising it buys margin against a slow lamp and pays for it
    // there.
    var RECONCILE_DELAY = 1000;

    function supersede(id, state) {
        // Only drop the guard when nothing is outstanding. An event that lands
        // mid-request is not necessarily newer than the request: Tasmota
        // publishes tele/<id>/STATE on a timer, and a frame sent just before
        // the command reached the lamp restates the state we are switching away
        // from. Dropping the guard there let a later resync write freely to a
        // device that still had a PUT in flight.
        if (!queued[id]) {
            delete inFlight[id];
        }
        epoch[id] = (epoch[id] || 0) + 1;
        eventSeq[id] = (eventSeq[id] || 0) + 1;
        if (burst[id]) {
            // An authoritative event is the newest thing known about this device,
            // exactly like a request that landed. Treating it as one means a burst
            // that then fails falls back to it, instead of being blocked from
            // falling back at all and leaving a state that never happened.
            burst[id].state = state;
            burst[id].succeeded = true;
            burst[id].failed = false;
        }
    }

    document.addEventListener("change", function (event) {
        var input = event.target;
        if (!input.matches || !input.matches("input[data-device]")) {
            return;
        }

        var row = input.closest(".device");
        var id = input.dataset.device;
        var target = input.checked;
        var token = {};

        // A burst is every tap on one device that overlaps in flight. Bumping the
        // epoch per tap is what stops an in-flight resync painting over the
        // optimistic state, but a tap that failed taught us nothing, and leaving
        // the epoch raised would disqualify the device from the very resync that
        // would have corrected it. Undoing only the newest tap is not enough:
        // with two failed taps the first one's bump would survive. So the burst
        // as a whole records where it started and unwinds to there if none of it
        // ever succeeded.
        if (!queued[id]) {
            burst[id] = {
                epoch: epoch[id] || 0,
                state: row.classList.contains("is-on"),
                succeeded: false,
                failed: false,
                // Set when an event's repaint was held back because a request
                // was in flight; the burst then reconciles against the server.
                suppressed: false
            };
        }

        inFlight[id] = token;
        epoch[id] = (epoch[id] || 0) + 1;
        queued[id] = (queued[id] || 0) + 1;

        // Show the new state right away, then correct it if the request fails:
        // waiting for the broker to echo it back makes the switch feel broken.
        paint(row, target);
        row.classList.add("is-pending");

        function send() {
            // What we knew when this request went out. Anything that arrives
            // after it is newer than its result.
            var atSend = eventSeq[id] || 0;

            return request("/api/" + encodeURIComponent(id), {
                method: "PUT",
                body: target ? "true" : "false"
            // The reply is empty, but leaving it unread makes the browser cancel
            // the body stream and log an aborted request every time.
            }, function (response) {
                return response.text();
            }).then(function () {
                // A request that landed is new knowledge about the device, so
                // it moves the epoch on, exactly as an event does. Without
                // this, a resync whose snapshot was taken after the tap but
                // answered before the lamp echoed - which is every resync
                // issued during an MQTT round trip - passed both guards in
                // applyStates and painted the switch back to where it started,
                // while the status pill said "Live".
                epoch[id] = (epoch[id] || 0) + 1;
                if (burst[id]) {
                    burst[id].succeeded = true;
                    burst[id].failed = false;
                    if ((eventSeq[id] || 0) === atSend) {
                        burst[id].state = target;
                    }
                    // Otherwise an event landed while this was in flight and
                    // already recorded what it said; a command that has only
                    // just been published does not get to overrule it.
                }
            }).catch(function (err) {
                var failure = row.querySelector(".device-name").textContent +
                    ": switch failed, " + describe(err);
                log(failure);
                announce(failure, true);
                if (burst[id]) {
                    burst[id].failed = true;
                }
            }).finally(function () {
                if (inFlight[id] === token) {
                    delete inFlight[id];
                }

                queued[id]--;
                if (queued[id] > 0) {
                    return;
                }

                row.classList.remove("is-pending");

                var b = burst[id];
                delete burst[id];
                if (!b) {
                    return;
                }

                if (b.suppressed) {
                    // Delayed on purpose. /api/state is fed by the same MQTT
                    // stream the suppressed frame came from, so asking the
                    // instant the request settles gets the pre-echo answer -
                    // which repaints the very state that was suppressed, half a
                    // second later instead of immediately. Waiting lets the
                    // device's echo land first, and if it does, this resync
                    // only confirms what the event already painted.
                    // An event arrived mid-request and its repaint was held
                    // back, because at that moment there was no way to tell
                    // whether it predated the command - Tasmota republishes
                    // the old state on a timer - or superseded it. Guessing
                    // either way is wrong half the time, so ask: /api/state is
                    // the server's own view once everything has landed, and
                    // applyStates has the epoch guard that stops a stale
                    // snapshot overwriting anything newer.
                    clearTimeout(reconcileTimers[id]);
                    reconcileTimers[id] = setTimeout(function () {
                        delete reconcileTimers[id];
                        resync();
                    }, RECONCILE_DELAY);
                }

                if (!b.failed) {
                    return;
                }

                // The last request of the burst failed, so the page is showing an
                // optimistic state that never happened. Fall back to the last one
                // that did - the start of the burst, or the last tap that landed.
                input.checked = b.state;
                paint(row, b.state);
                // The switch snapping back on its own is otherwise the only
                // sign of a failure, and the reason for it is written to a log
                // panel that is closed by default.
                clearTimeout(failedTimers[id]);
                row.classList.remove("is-failed");
                // Reading offsetWidth restarts the animation; without it a
                // second failure within the window is not shown at all.
                void row.offsetWidth;
                row.classList.add("is-failed");
                failedTimers[id] = setTimeout(function () {
                    row.classList.remove("is-failed");
                    delete failedTimers[id];
                }, 2000);
                if (!b.succeeded) {
                    epoch[id] = b.epoch;
                }
            });
        }

        chain[id] = (chain[id] || Promise.resolve()).then(send, send);
    });

    // --- Activity log ------------------------------------------------------

    function log(text, timestamp) {
        if (!logEl) {
            return;
        }

        var placeholder = logEl.querySelector(".log-empty");
        if (placeholder) {
            placeholder.remove();
        }

        var when = timestamp ? new Date(timestamp) : new Date();
        var entry = document.createElement("div");
        entry.className = "log-entry";

        var time = document.createElement("span");
        time.className = "log-time";
        time.textContent = isNaN(when.getTime()) ? "--:--:--" : when.toLocaleTimeString();
        entry.appendChild(time);
        entry.appendChild(document.createTextNode(text));

        // While the panel is collapsed, engines that hide it with display:none
        // report every metric as zero. Pinning on those numbers scrolls the log to
        // the top and then wedges it there, because from that point on the
        // at-bottom test can never be true again. Skip the pin and do it on open.
        var measurable = logEl.clientHeight > 0;
        var atBottom = measurable && logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 4;

        logEl.appendChild(entry);

        while (logEl.children.length > LOG_LIMIT) {
            logEl.removeChild(logEl.firstChild);
        }
        if (atBottom) {
            logEl.scrollTop = logEl.scrollHeight;
        }
    }

    var logDetails = document.querySelector(".log");
    if (logDetails) {
        logDetails.addEventListener("toggle", function () {
            if (logDetails.open) {
                logEl.scrollTop = logEl.scrollHeight;
            }
        });
    }

    // --- Resync -------------------------------------------------------------

    // The template renders the state as it was when the page was built, and the
    // websocket only carries changes from the moment it is open. Everything in
    // between - the gap before the socket connects, and the whole of any outage -
    // is invisible to the page, so refetch the truth whenever the socket comes up.
    var resyncSeq = 0;
    var resyncRetry = 0;
    var resyncTimer = null;
    var socketOpen = false;

    function applyStates(states, seen) {
        Object.keys(states).forEach(function (id) {
            // Skip a device with a request in flight, and one that something told
            // us about after this snapshot was taken - a websocket event, or the
            // user's own tap. Either is newer than what we asked for.
            if (id in inFlight || (epoch[id] || 0) !== (seen[id] || 0)) {
                return;
            }
            var row = rowFor(id);
            if (!row) {
                return;
            }
            var state = !!states[id];
            row.querySelector("input[data-device]").checked = state;
            paint(row, state);
        });
    }

    // resync fetches the authoritative state and reconciles the page against it.
    //
    // Until it succeeds the page is showing state it cannot vouch for, so the
    // status pill does not say "Live" yet - claiming to be live over stale rows is
    // the exact symptom the resync exists to prevent. A failure is retried with
    // the same backoff the socket uses, because one failed fetch would otherwise
    // strand the page in that state until the socket happened to drop.
    function resync() {
        var seq = ++resyncSeq;
        var seen = Object.assign(Object.create(null), epoch);

        return request("/api/state", null, function (response) {
            return response.json();
        }).then(function (states) {
            // A newer resync is already in flight and owns the outcome, status
            // included; this snapshot is older than the one about to replace it.
            if (seq !== resyncSeq) {
                return;
            }
            applyStates(states, seen);
            resyncRetry = 0;
            if (socketOpen) {
                setStatus("live", "Live");
            }
        }).catch(function (err) {
            if (seq !== resyncSeq) {
                return;
            }
            log("could not read device state: " + describe(err));
            if (!socketOpen) {
                return;
            }
            setStatus("down", "Out of sync");
            var delay = Math.min(1000 * Math.pow(2, resyncRetry++), 30000);
            clearTimeout(resyncTimer);
            resyncTimer = setTimeout(resync, delay);
        });
    }

    // --- Websocket ---------------------------------------------------------

    var retry = 0;
    var socket = null;
    var reconnectTimer = null;

    // lastContact is when the socket last proved it was alive. A socket that
    // dies without a close frame - a phone in a pocket, a router that dropped
    // the NAT entry - leaves onclose unfired and the page claiming "Live" over
    // rows that stopped updating, so returning to the tab checks the age of
    // this rather than trusting socketOpen.
    var lastContact = 0;

    // The server sends a heartbeat every wsPingInterval (30s), so a healthy
    // socket proves itself twice inside this window: one lost heartbeat leaves
    // the age at exactly 60s and the test below is strictly greater, so it
    // takes two consecutive losses - 60s of real silence - to declare the
    // socket dead. That is one heartbeat of slack, which is the minimum worth
    // having: shortening the interval or lengthening this window widens it.
    var STALE_AFTER = 60000;

    // reconnectNow reconnects on the next tick, for when something has just
    // told us the network is back or the socket is gone.
    function reconnectNow() {
        setStatus("down", "Reconnecting");
        clearTimeout(reconnectTimer);
        reconnectTimer = setTimeout(connect, 0);
    }

    function reconnectLater() {
        // Back off up to 30s so a server that stays down is not hammered.
        var delay = Math.min(1000 * Math.pow(2, retry++), 30000);
        setStatus("down", "Reconnecting");
        clearTimeout(reconnectTimer);
        reconnectTimer = setTimeout(connect, delay);
    }

    function connect() {
        var scheme = document.location.protocol === "https:" ? "wss://" : "ws://";
        try {
            socket = new WebSocket(scheme + document.location.host + "/api/ws");
        } catch (err) {
            // new WebSocket() throws rather than failing asynchronously for a
            // malformed or blocked URL. Without this the reconnect chain, which
            // is only ever re-entered from onclose, would end here for good.
            socket = null;
            log("could not open websocket: " + err.message);
            reconnectLater();
            return;
        }

        var self = socket;

        socket.onopen = function () {
            socketOpen = true;
            lastContact = Date.now();

            // Only reset the backoff once the connection has proved it can
            // stay up. Resetting it on open meant a server that accepts and
            // immediately drops - which this one does to a client that falls
            // 64 events behind - was reconnected once a second forever, with
            // the exponential backoff never engaging.
            setTimeout(function () {
                if (socket === self && socketOpen) {
                    retry = 0;
                }
            }, 10000);

            // Not "Live" yet: the socket only carries changes from this moment on,
            // so the rows are unverified until the resync below comes back.
            setStatus("connecting", "Syncing");
            resyncRetry = 0;
            clearTimeout(resyncTimer);
            resync();
        };

        socket.onmessage = function (message) {
            var event;
            try {
                event = JSON.parse(message.data);
            } catch (err) {
                log("unparseable event: " + message.data);
                return;
            }

            lastContact = Date.now();

            if (event.Heartbeat) {
                // Proof the socket is alive while nothing is happening, which
                // on a lamp dashboard is most of the time. Nothing else to do.
                return;
            }

            if (!event.Payload || typeof event.Payload.State !== "boolean") {
                // Not a switch event. Coercing an unknown payload with !! would
                // paint the row off, which for a sensor reading is a lie.
                log("ignoring event for " + event.DeviceId + " with an unknown payload");
                return;
            }

            var state = event.Payload.State;
            supersede(event.DeviceId, state);

            var row = rowFor(event.DeviceId);
            // Not while a request is outstanding: this event may be the
            // telemetry frame that restates the state we are switching away
            // from, and repainting from it makes the switch bounce back while
            // the lamp is on its way. The burst resolves it against the server
            // when it settles - see "suppressed" in the change handler.
            if (row && !queued[event.DeviceId]) {
                row.querySelector("input[data-device]").checked = state;
                paint(row, state);
            } else if (burst[event.DeviceId]) {
                burst[event.DeviceId].suppressed = true;
            }

            var name = row ? row.querySelector(".device-name").textContent : event.DeviceId;
            announce(name + " " + (state ? "on" : "off"));
            // "->" rather than an arrow glyph: U+2192 is outside both Inter
            // subsets, so it would render from the fallback stack on every line.
            log(name + " -> " + (state ? "on" : "off"), event.Timestamp);
        };

        socket.onclose = function () {
            if (socket === self) {
                socket = null;
            }
            socketOpen = false;
            // Retrying /api/state while the socket is down is pointless; the next
            // onopen starts a fresh one.
            clearTimeout(resyncTimer);

            // The server restarts on config changes and the phone drops the
            // socket whenever it sleeps, so reconnecting is the normal path,
            // not an error path.
            reconnectLater();
        };

        socket.onerror = function () {
            // self, not the module-level socket, which may already have been
            // replaced by a newer connection.
            self.close();
        };
    }

    // Coming back to the page is the moment the rows are most likely to be
    // stale, and the moment a half-open socket is most likely to be discovered.
    // A short absence only needs the state refetched; a long one gets a fresh
    // socket, because the old one may be talking to nobody.
    function recheck() {
        if (!socketOpen) {
            // Coming back to a page that is waiting out a backoff should not
            // mean waiting out the rest of it. The backoff itself is left
            // alone: it is reset by a connection that stays up, and resetting
            // it here would let a phone handing off between wifi and cellular
            // - which fires "online" repeatedly - hammer a server that is down.
            if (socket === null) {
                reconnectNow();
            }
            return;
        }

        if (Date.now() - lastContact > STALE_AFTER && socket) {
            // Take the reconnect over rather than leaving it to onclose. A
            // socket that died without a FIN never completes its closing
            // handshake, and Chromium then sits in CLOSING for tens of seconds
            // without firing onclose - during which the old code would call
            // close() again on every check and never reach the resync below,
            // so the page stayed frozen on "Live" with nothing reconnecting.
            var dying = socket;
            socket = null;
            socketOpen = false;
            dying.onclose = null;
            dying.onmessage = null;
            dying.onerror = null;
            dying.close();
            reconnectNow();
            return;
        }

        resync();
    }

    // Coalesced: a bfcache restore fires pageshow and visibilitychange
    // together, which would otherwise be two refetches of the same thing.
    var recheckTimer = null;

    function recheckSoon() {
        clearTimeout(recheckTimer);
        recheckTimer = setTimeout(recheck, 50);
    }

    document.addEventListener("visibilitychange", function () {
        if (document.visibilityState === "visible") {
            recheckSoon();
        }
    });
    window.addEventListener("online", recheckSoon);
    window.addEventListener("pageshow", recheckSoon);

    // The switches render disabled, so that a page whose script never ran
    // cannot animate a toggle that talks to nobody. This is the last statement
    // of initialisation for that reason: anything above it that throws leaves
    // them disabled, which is honest.
    function enableSwitches() {
        document.querySelectorAll("input[data-device]").forEach(function (input) {
            input.disabled = false;
        });
    }

    refreshCounts();
    setStatus("connecting", "Connecting");
    connect();
    enableSwitches();
})();
