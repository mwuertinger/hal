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

    // fetchWithTimeout bounds a request the way the server cannot. A phone that
    // walks out of WiFi mid-request leaves the socket open with nothing coming
    // back, and without this the promise never settles: the switch stays pending
    // and the device's chain stops accepting taps.
    function fetchWithTimeout(url, options) {
        var abort = new AbortController();
        var timer = setTimeout(function () {
            abort.abort();
        }, REQUEST_TIMEOUT);

        options = options || {};
        options.signal = abort.signal;

        return fetch(url, options).finally(function () {
            clearTimeout(timer);
        });
    }

    // --- Switching ---------------------------------------------------------

    // One token per device while a PUT is in flight. Anything that learns the real
    // state in the meantime - a websocket event, a newer click - drops the token,
    // which is what stops a late failure from reverting to a state that is no
    // longer the one we started from.
    var inFlight = Object.create(null);

    // Bumped whenever something authoritative is applied to a device. A resync
    // compares this against the value it captured before its request went out, so
    // a snapshot that was already stale when it arrived cannot overwrite a newer
    // fact. See resync().
    var epoch = Object.create(null);

    // One promise chain per device, so two taps cannot race each other to the
    // broker. Without this the second PUT can overtake the first and leave the
    // lamp in the state the user did not ask for, with the page none the wiser.
    var chain = Object.create(null);

    // How many requests are outstanding per device, so the pending state clears
    // when the last one settles rather than when the newest token happens to win.
    var queued = Object.create(null);

    function supersede(id) {
        delete inFlight[id];
        epoch[id] = (epoch[id] || 0) + 1;
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

        // Kept so the revert below can put it back. Bumping at tap time is what
        // stops an in-flight resync from painting over the optimistic state, but
        // if the tap turns out to have failed then nothing was learned, and
        // leaving the epoch raised would disqualify this device from the very
        // resync that would have corrected it.
        var epochBefore = epoch[id] || 0;

        inFlight[id] = token;
        epoch[id] = epochBefore + 1;
        queued[id] = (queued[id] || 0) + 1;

        // Show the new state right away, then correct it if the request fails:
        // waiting for the broker to echo it back makes the switch feel broken.
        paint(row, target);
        row.classList.add("is-pending");

        function send() {
            return fetchWithTimeout("/api/" + encodeURIComponent(id), {
                method: "PUT",
                body: target ? "true" : "false"
            }).then(function (response) {
                if (!response.ok) {
                    throw new Error("HTTP " + response.status);
                }
                // The reply is empty, but leaving it unread makes the browser
                // cancel the body stream and log an aborted request every time.
                return response.text();
            }).catch(function (err) {
                log(row.querySelector(".device-name").textContent +
                    ": switch failed, " + err.message);

                // Only undo our own optimistic paint. If something already told us
                // the real state, or the user has since clicked again, that is the
                // truth now and this stale response must not overwrite it.
                if (inFlight[id] !== token) {
                    return;
                }
                input.checked = !target;
                paint(row, !target);

                // Still the newest token, so nothing has bumped the epoch since
                // this tap did; putting it back cannot discard anyone else's fact.
                epoch[id] = epochBefore;
            }).finally(function () {
                if (inFlight[id] === token) {
                    delete inFlight[id];
                }
                queued[id]--;
                if (queued[id] === 0) {
                    row.classList.remove("is-pending");
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

        return fetchWithTimeout("/api/state").then(function (response) {
            if (!response.ok) {
                throw new Error("HTTP " + response.status);
            }
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
            log("could not read device state: " + err.message);
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

            supersede(event.DeviceId);

            var row = rowFor(event.DeviceId);
            if (row) {
                var state = !!(event.Payload && event.Payload.State);
                row.querySelector("input[hal-device]").checked = state;
                paint(row, state);
            }

            var name = row ? row.querySelector(".device-name").textContent : event.DeviceId;
            log(name + " → " + (event.Payload && event.Payload.State ? "on" : "off"), event.Timestamp);
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
