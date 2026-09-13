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
        if (textEl.textContent !== text) {
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
            room.querySelector(".room-count").textContent = roomOn + "/" + rooms.length + " on";

            total += rooms.length;
            on += roomOn;
        });

        if (overviewEl) {
            overviewEl.innerHTML = total === 0 ? "" : "<b>" + on + "</b> of " + total + " on";
        }
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

    function supersede(id, state) {
        delete inFlight[id];
        epoch[id] = (epoch[id] || 0) + 1;
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
        if (!input.matches || !input.matches("input[hal-device]")) {
            return;
        }

        var row = input.closest(".device");
        var id = input.getAttribute("hal-device");
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
                failed: false
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
            return request("/api/" + encodeURIComponent(id), {
                method: "PUT",
                body: target ? "true" : "false"
            // The reply is empty, but leaving it unread makes the browser cancel
            // the body stream and log an aborted request every time.
            }, function (response) {
                return response.text();
            }).then(function () {
                if (burst[id]) {
                    burst[id].succeeded = true;
                    burst[id].failed = false;
                    burst[id].state = target;
                }
            }).catch(function (err) {
                log(row.querySelector(".device-name").textContent +
                    ": switch failed, " + describe(err));
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
                if (!b || !b.failed) {
                    return;
                }

                // The last request of the burst failed, so the page is showing an
                // optimistic state that never happened. Fall back to the last one
                // that did - the start of the burst, or the last tap that landed.
                input.checked = b.state;
                paint(row, b.state);
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
            row.querySelector("input[hal-device]").checked = state;
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

    function connect() {
        var scheme = document.location.protocol === "https:" ? "wss://" : "ws://";
        var socket = new WebSocket(scheme + document.location.host + "/api/ws");

        socket.onopen = function () {
            retry = 0;
            socketOpen = true;

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

            var state = !!(event.Payload && event.Payload.State);
            supersede(event.DeviceId, state);

            var row = rowFor(event.DeviceId);
            if (row) {
                row.querySelector("input[hal-device]").checked = state;
                paint(row, state);
            }

            var name = row ? row.querySelector(".device-name").textContent : event.DeviceId;
            // "->" rather than an arrow glyph: U+2192 is outside both Inter
            // subsets, so it would render from the fallback stack on every line.
            log(name + " -> " + (state ? "on" : "off"), event.Timestamp);
        };

        socket.onclose = function () {
            socketOpen = false;
            // Retrying /api/state while the socket is down is pointless; the next
            // onopen starts a fresh one.
            clearTimeout(resyncTimer);

            // The server restarts on config changes and the phone drops the
            // socket whenever it sleeps, so reconnecting is the normal path,
            // not an error path. Back off up to 30s so a server that stays
            // down does not get hammered.
            var delay = Math.min(1000 * Math.pow(2, retry++), 30000);
            setStatus("down", "Reconnecting");
            setTimeout(connect, delay);
        };

        socket.onerror = function () {
            socket.close();
        };
    }

    refreshCounts();
    setStatus("connecting", "Connecting");
    connect();
})();
