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
        statusEl.querySelector(".status-text").textContent = text;
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

    // --- Switching ---------------------------------------------------------

    document.addEventListener("change", function (event) {
        var input = event.target;
        if (!input.matches || !input.matches("input[hal-device]")) {
            return;
        }

        var row = input.closest(".device");
        var target = input.checked;

        // Show the new state right away, then correct it if the request fails:
        // waiting for the broker to echo it back makes the switch feel broken.
        paint(row, target);
        row.classList.add("is-pending");
        input.disabled = true;

        fetch("/api/" + encodeURIComponent(input.getAttribute("hal-device")), {
            method: "PUT",
            body: target ? "true" : "false"
        }).then(function (response) {
            if (!response.ok) {
                throw new Error("HTTP " + response.status);
            }
            // The reply is empty, but leaving it unread makes the browser cancel
            // the body stream and log an aborted request for every toggle.
            return response.text();
        }).catch(function (err) {
            input.checked = !target;
            paint(row, !target);
            log("switch failed: " + err.message);
        }).finally(function () {
            input.disabled = false;
            row.classList.remove("is-pending");
        });
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

        var atBottom = logEl.scrollTop + logEl.clientHeight >= logEl.scrollHeight - 4;
        logEl.appendChild(entry);

        while (logEl.children.length > LOG_LIMIT) {
            logEl.removeChild(logEl.firstChild);
        }
        if (atBottom) {
            logEl.scrollTop = logEl.scrollHeight;
        }
    }

    // --- Websocket ---------------------------------------------------------

    var retry = 0;

    function connect() {
        var scheme = document.location.protocol === "https:" ? "wss://" : "ws://";
        var socket = new WebSocket(scheme + document.location.host + "/api/ws");

        socket.onopen = function () {
            retry = 0;
            setStatus("live", "Live");
        };

        socket.onmessage = function (message) {
            var event;
            try {
                event = JSON.parse(message.data);
            } catch (err) {
                log("unparseable event: " + message.data);
                return;
            }

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
