package frontend

import (
	"encoding/json"
	"errors"
	"io"
	"net"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/device"
	"github.com/mwuertinger/hal/pkg/mqtt"
)

// testDevices registers two switches against a fake broker, so the handlers
// have something to render. The device registry is package state, so this runs
// once for the whole binary rather than per test.
var testBroker = func() *mqtt.Fake {
	broker := mqtt.NewFake()
	device.SetMqttBroker(broker)
	err := device.RegisterDevices([]config.Device{
		{ID: "lamp1", Name: "Floor Lamp", Location: "Living Room", Type: config.DeviceTypeSonoffMqttSwitch},
		{ID: "lamp2", Name: "Desk Lamp", Location: "Office", Type: config.DeviceTypeSonoffMqttSwitch},
	})
	if err != nil {
		panic(err)
	}
	return broker
}()

func newServer(t *testing.T) *httptest.Server {
	t.Helper()

	srv := httptest.NewServer(hostCheck(router(), nil))
	t.Cleanup(srv.Close)
	return srv
}

// do issues a request without following redirects, so a 301 is visible.
func do(t *testing.T, method, url string, body string) *http.Response {
	t.Helper()

	req, err := http.NewRequest(method, url, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatalf("%s %s: %v", method, url, err)
	}
	t.Cleanup(func() { resp.Body.Close() })
	return resp
}

func cssURL(t *testing.T) string {
	t.Helper()
	u, err := assetURL("css/hal.css")
	if err != nil {
		t.Fatal(err)
	}
	return u
}

// TestStaticCaching covers the headers the content-addressed scheme depends on.
// Losing any of them re-opens the incident it was built for: a browser that had
// cached hal.css under a stable URL went on applying the old stylesheet to new
// markup for weeks, because a response with Last-Modified and no Cache-Control
// is heuristically fresh for a tenth of its age and is never revalidated.
func TestStaticCaching(t *testing.T) {
	srv := newServer(t)

	resp := do(t, "GET", srv.URL+cssURL(t), "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got, want := resp.Header.Get("Cache-Control"), "public, max-age=31536000, immutable"; got != want {
		t.Errorf("Cache-Control = %q, want %q", got, want)
	}
	if got, want := resp.Header.Get("Content-Type"), "text/css; charset=utf-8"; got != want {
		t.Errorf("Content-Type = %q, want %q", got, want)
	}
	if resp.Header.Get("ETag") == "" {
		t.Error("no ETag")
	}
	if resp.Header.Get("Last-Modified") != "" {
		t.Error("Last-Modified is set, which is what makes a response heuristically cacheable")
	}
}

func TestStaticNotFoundIsNotCacheable(t *testing.T) {
	srv := newServer(t)

	resp := do(t, "GET", srv.URL+"/static/css/nope.css", "")
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("status = %d, want 404", resp.StatusCode)
	}
	// A 404 is heuristically cacheable too, and these URLs never change back.
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}
}

// TestUnsafeMethodsAreRejected: a 2xx to an unsafe method makes a cache
// invalidate the entry (RFC 9111 4.4), so a cross-origin POST - which, unlike
// PUT, needs no preflight - could evict the very asset immutable exists to
// keep. The static route used to answer every method with the file.
func TestUnsafeMethodsAreRejected(t *testing.T) {
	srv := newServer(t)
	css := cssURL(t)

	for _, tc := range []struct {
		method, path string
		want         int
	}{
		{"GET", css, 200},
		{"HEAD", css, 200},
		{"POST", css, 405},
		{"PUT", css, 405},
		{"DELETE", css, 405},
		{"GET", "/", 200},
		{"HEAD", "/", 200},
		{"POST", "/", 405},
		{"GET", "/api/state", 200},
		{"HEAD", "/api/state", 200},
		{"POST", "/api/state", 405},
	} {
		resp := do(t, tc.method, srv.URL+tc.path, "")
		if resp.StatusCode != tc.want {
			t.Errorf("%s %s = %d, want %d", tc.method, tc.path, resp.StatusCode, tc.want)
		}
	}
}

// TestUnknownPathIs404 pins "GET /{$}": without the {$} the root pattern is a
// catch-all prefix, and every unknown URL would answer 200 with the home page.
func TestUnknownPathIs404(t *testing.T) {
	srv := newServer(t)

	for _, path := range []string{"/nope", "/api", "/api/", "/deep/path"} {
		resp := do(t, "GET", srv.URL+path, "")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("GET %s = %d, want 404", path, resp.StatusCode)
		}
	}
}

// TestDeviceIdComesFromThePathValue covers the mux.Vars replacement: the id has
// to be the decoded final segment and nothing else.
func TestDeviceIdComesFromThePathValue(t *testing.T) {
	srv := newServer(t)

	// A device id that is not registered must 404 from the lookup, whatever it
	// contains - and must not be mistaken for a longer path.
	for _, path := range []string{"/api/a%2Fb", "/api/%20x", "/api/lamp1%0Aforged"} {
		resp := do(t, "PUT", srv.URL+path, "true")
		if resp.StatusCode != http.StatusNotFound {
			t.Errorf("PUT %s = %d, want 404", path, resp.StatusCode)
		}
	}

	// And the registered one still resolves.
	if resp := do(t, "PUT", srv.URL+"/api/lamp1", "true"); resp.StatusCode != http.StatusOK {
		t.Errorf("PUT /api/lamp1 = %d, want 200", resp.StatusCode)
	}

	// A sub-path is not a device: {device} must not match across a separator.
	// The router's 404 carries net/http's own body; the handler's is empty, so
	// the body is what says which of the two answered.
	resp := do(t, "PUT", srv.URL+"/api/lamp1/extra", "true")
	if resp.StatusCode != http.StatusNotFound {
		t.Errorf("PUT /api/lamp1/extra = %d, want 404", resp.StatusCode)
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	if len(body) == 0 {
		t.Error("PUT /api/lamp1/extra was answered by the handler, so {device} matched across a separator")
	}
}

func TestHomeIsNotCacheable(t *testing.T) {
	srv := newServer(t)

	resp := do(t, "GET", srv.URL+"/", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store; the page carries live device state", got)
	}
	if resp.Header.Get("Last-Modified") != "" {
		t.Error("Last-Modified on the live-state page makes it heuristically cacheable")
	}
}

func TestStateHandler(t *testing.T) {
	srv := newServer(t)

	resp := do(t, "GET", srv.URL+"/api/state", "")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status = %d, want 200", resp.StatusCode)
	}
	if got := resp.Header.Get("Cache-Control"); got != "no-store" {
		t.Errorf("Cache-Control = %q, want no-store", got)
	}

	var states map[string]bool
	if err := json.NewDecoder(resp.Body).Decode(&states); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"lamp1", "lamp2"} {
		if _, ok := states[id]; !ok {
			t.Errorf("state for %q is missing: %v", id, states)
		}
	}
}

func TestSwitchHandler(t *testing.T) {
	srv := newServer(t)

	for _, tc := range []struct {
		name, path, body string
		want             int
	}{
		{"on", "/api/lamp1", "true", 200},
		{"off", "/api/lamp1", "false", 200},
		{"unknown device", "/api/nosuchlamp", "true", 404},
		{"unparseable body", "/api/lamp1", "yes", 400},
		{"empty body", "/api/lamp1", "", 400},
		// Without the bound, ReadTimeout caps how long a body may take to
		// arrive but nothing caps how much of it there is.
		{"oversized body", "/api/lamp1", strings.Repeat("t", 200), 413},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := do(t, "PUT", srv.URL+tc.path, tc.body)
			if resp.StatusCode != tc.want {
				t.Errorf("PUT %s %q = %d, want %d", tc.path, tc.body, resp.StatusCode, tc.want)
			}
		})
	}

	// A rejected body must not have reached the lamp.
	for _, p := range testBroker.Published() {
		if p.Topic == "cmnd/lamp1/POWER" && p.Msg != "" && p.Msg != "0" && p.Msg != "1" {
			t.Errorf("published %q to %s", p.Msg, p.Topic)
		}
	}
}

func TestSwitchHandlerPublishFailureIsReported(t *testing.T) {
	srv := newServer(t)

	testBroker.PublishErr = errFake
	defer func() { testBroker.PublishErr = nil }()

	resp := do(t, "PUT", srv.URL+"/api/lamp1", "true")
	if resp.StatusCode != http.StatusInternalServerError {
		t.Errorf("status = %d, want 500 so the switch does not claim success", resp.StatusCode)
	}
}

var errFake = &fakeError{}

type fakeError struct{}

func (*fakeError) Error() string { return "broker unavailable" }

// TestHostCheck covers the DNS-rebinding guard: HAL has no authentication, so
// the only thing standing between a public page and the lamps is that the
// browser sends the name the user typed in the Host header.
func TestHostCheck(t *testing.T) {
	permitted := map[string]bool{"hal.example.com": true}

	for _, tc := range []struct {
		host string
		want bool
	}{
		{"192.168.178.2:8080", true},
		{"192.168.178.2", true},
		{"[fd00::1]:8080", true},
		{"[fd00::1]", true},
		{"localhost:8080", true},
		{"raspberrypi:8080", true},
		{"hal.local", true},
		{"hal.localdomain", true},
		// .box is a delegated ICANN gTLD, unlike every other suffix here, so
		// only the name AVM actually holds is trusted. Trusting the TLD would
		// let anyone who registers a .box domain rebind straight into HAL.
		{"fritz.box", true},
		{"hal.fritz.box", true},
		{"rebind.box", false},
		{"evil.notfritz.box", false},
		{"hal.lan:8080", true},
		{"hal.home.arpa", true},
		{"hal.example.com", true}, // allow-listed
		{"HAL.EXAMPLE.COM", true}, // and case-insensitively
		{"evil.example:8080", false},
		{"rebind.attacker.test", false},
		{"", false},
	} {
		if got := hostIsLocal(tc.host, permitted); got != tc.want {
			t.Errorf("hostIsLocal(%q) = %v, want %v", tc.host, got, tc.want)
		}
	}
}

func TestHostCheckRejectsForeignHost(t *testing.T) {
	srv := newServer(t)

	req, err := http.NewRequest("GET", srv.URL+"/api/state", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "rebind.attacker.test"

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("status = %d, want 421", resp.StatusCode)
	}
}

// notASwitch is a Device that cannot be switched, which is what the
// Device/Switch split is for. Adding a sensor is the realistic way this arrives.
type notASwitch struct{}

func (notASwitch) ID() string                  { return "sensor1" }
func (notASwitch) Name() string                { return "Hallway Sensor" }
func (notASwitch) Location() string            { return "Hallway" }
func (notASwitch) Events() <-chan device.Event { return nil }
func (notASwitch) Shutdown()                   {}

// TestBuildHomePageSkipsNonSwitches: homeHandler used to assert d.(device.Switch)
// unchecked, so the first non-switch device turned every page load into a panic
// and a dead page - while stateHandler, which checks, quietly omitted it.
func TestBuildHomePageSkipsNonSwitches(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("a non-switch device panicked the home page: %v", r)
		}
	}()

	page := buildHomePage(append(device.List(), notASwitch{}))

	for _, room := range page.Rooms {
		if room.Name == "Hallway" {
			t.Errorf("the non-switch device was rendered as a switch: %+v", room)
		}
	}
	if page.Total != len(device.List()) {
		t.Errorf("Total = %d, want %d", page.Total, len(device.List()))
	}
}

// --- websocket -------------------------------------------------------------

func wsDial(t *testing.T, srv *httptest.Server, origin string) (*websocket.Conn, *http.Response, error) {
	t.Helper()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "ws"
	u.Path = "/api/ws"

	header := http.Header{}
	if origin != "" {
		header.Set("Origin", origin)
	}
	return websocket.DefaultDialer.Dial(u.String(), header)
}

// TestWebsocketRejectsForeignOrigin: websockets are exempt from the same-origin
// policy and from CORS preflight, so the deprecated websocket.Upgrade - which
// accepts every origin - let any page the user visited read the event stream,
// which is a timestamped record of movement through the house.
func TestWebsocketRejectsForeignOrigin(t *testing.T) {
	srv := newServer(t)

	conn, resp, err := wsDial(t, srv, "http://evil.example")
	if err == nil {
		conn.Close()
		t.Fatal("a cross-origin websocket handshake was accepted")
	}
	if resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("status = %v, want 403", resp)
	}
}

// TestHostCheckCoversTheWebsocket: the middleware wraps the whole router, and
// the websocket is the route where a rebinding attacker gets the most - the
// live event stream. Exempting it would be silent.
func TestHostCheckCoversTheWebsocket(t *testing.T) {
	srv := newServer(t)

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	req, err := http.NewRequest("GET", srv.URL+"/api/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	req.Host = "rebind.attacker.test"
	req.Header.Set("Connection", "Upgrade")
	req.Header.Set("Upgrade", "websocket")
	req.Header.Set("Sec-WebSocket-Version", "13")
	req.Header.Set("Sec-WebSocket-Key", "dGhlIHNhbXBsZSBub25jZQ==")
	req.Header.Set("Origin", "http://"+u.Host)

	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusMisdirectedRequest {
		t.Errorf("websocket upgrade with a foreign Host = %d, want 421", resp.StatusCode)
	}
}

func TestWebsocketAcceptsSameOrigin(t *testing.T) {
	srv := newServer(t)

	waitClients(t, 0)

	conn, _, err := wsDial(t, srv, srv.URL)
	if err != nil {
		t.Fatalf("same-origin handshake refused: %v", err)
	}
	defer conn.Close()

	waitClients(t, 1)
}

// TestWebsocketReadLimit: ReadMessage buffers a whole message before it can be
// discarded, and gorilla's default limit is unlimited, so one client frame
// could be sized to exhaust the memory of a Raspberry Pi.
func TestWebsocketReadLimit(t *testing.T) {
	srv := newServer(t)

	waitClients(t, 0)

	conn, _, err := wsDial(t, srv, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	if err := conn.WriteMessage(websocket.TextMessage, make([]byte, wsReadLimit*4)); err != nil {
		t.Fatalf("write: %v", err)
	}

	// The server answers an oversized message with close 1009 and hangs up.
	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	_, _, err = conn.ReadMessage()
	if !websocket.IsCloseError(err, websocket.CloseMessageTooBig) {
		t.Errorf("read after an oversized frame = %v, want close 1009", err)
	}
}

func TestWebsocketClientLimit(t *testing.T) {
	srv := newServer(t)

	waitClients(t, 0)

	var conns []*websocket.Conn
	defer func() {
		for _, c := range conns {
			c.Close()
		}
	}()

	for i := 0; i < wsMaxClients; i++ {
		conn, _, err := wsDial(t, srv, srv.URL)
		if err != nil {
			t.Fatalf("connection %d refused: %v", i, err)
		}
		conns = append(conns, conn)
	}

	// Dialling returns as soon as the 101 is read, and registration happens
	// several statements later, so without this the client past the ceiling can
	// arrive while the map still has room.
	waitClients(t, wsMaxClients)

	// The handshake still succeeds - the ceiling is enforced after the upgrade
	// - but the connection is closed immediately with 1013 Try Again Later.
	conn, _, err := wsDial(t, srv, srv.URL)
	if err != nil {
		return // refused outright is fine too
	}
	conns = append(conns, conn)

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	if _, _, err := conn.ReadMessage(); !websocket.IsCloseError(err, websocket.CloseTryAgainLater) {
		t.Errorf("client %d past the ceiling was not turned away: %v", wsMaxClients+1, err)
	}
	if got := clientCount(); got > wsMaxClients {
		t.Errorf("registered clients = %d, want at most %d", got, wsMaxClients)
	}
}

// TestWebsocketDeliversEvents drives the broadcaster end to end: a device
// notification from the broker has to reach a connected browser.
func TestWebsocketDeliversEvents(t *testing.T) {
	shutdown = make(chan interface{})

	wg.Add(1)
	go broadcast(device.Events())
	defer func() {
		close(shutdown)
		wg.Wait()
	}()

	srv := newServer(t)

	// httptest.Server.Close() does not wait for hijacked connections, so the
	// previous test's clients may still be registered. Without this the dial
	// below can be refused at the ceiling, or waitClients(1) can be satisfied
	// by a leftover on its way out while this client is not yet registered.
	waitClients(t, 0)

	conn, _, err := wsDial(t, srv, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	waitClients(t, 1)

	testBroker.Deliver("stat/lamp1/POWER", "ON")

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	for {
		var event device.Event
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("no event reached the browser: %v", err)
		}
		// A heartbeat decodes into an Event with no DeviceId. The two shapes are
		// disjoint, but only the events are this test's business.
		if event.DeviceId == "" {
			continue
		}
		if event.DeviceId != "lamp1" {
			t.Errorf("DeviceId = %q, want lamp1", event.DeviceId)
		}
		break
	}
}

// waitClients waits for the registered client count to reach want. Closing a
// websocket is asynchronous - the reader goroutine has to notice - so asserting
// on the count directly is a race against the previous test's cleanup.
func waitClients(t *testing.T, want int) {
	t.Helper()

	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if clientCount() == want {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("registered clients = %d, want %d", clientCount(), want)
}

// clientCountWithin reads the client count, failing rather than hanging if the
// lock is held. A blocked broadcaster parks on its send while holding
// wsConnectionsMu, so an unbounded read here wedges the test goroutine before
// it can reach any assertion - and the failure surfaces as a whole-binary
// timeout minutes later, pointing at the wrong line.
func clientCountWithin(t *testing.T, d time.Duration) int {
	t.Helper()

	got := make(chan int, 1)
	go func() { got <- clientCount() }()

	select {
	case n := <-got:
		return n
	case <-time.After(d):
		t.Fatal("wsConnectionsMu is still held: the broadcaster blocked on a client that stopped reading")
		return -1
	}
}

func clientCount() int {
	wsConnectionsMu.Lock()
	defer wsConnectionsMu.Unlock()
	return len(wsConnections)
}

// TestShutdownWithoutStart and its sibling pin that the lifecycle is not a trap
// for a future early-exit path: both used to panic on a nil or closed channel.
func TestShutdownWithoutStart(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Shutdown() before Start() panicked: %v", r)
		}
	}()
	Shutdown()
}

func TestShutdownTwice(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Shutdown() twice panicked: %v", r)
		}
	}()

	if err := Start(config.Http{ListenAddress: "127.0.0.1:0"}); err != nil {
		t.Fatal(err)
	}
	Shutdown()
	Shutdown()

	// And Start works again afterwards, rather than reporting "already started"
	// forever.
	if err := Start(config.Http{ListenAddress: "127.0.0.1:0"}); err != nil {
		t.Fatalf("Start() after Shutdown(): %v", err)
	}
	Shutdown()
}

// TestWebsocketClosesTheConnection is the fd-leak regression test. Nothing else
// closes a hijacked connection: leaving it open costs a file descriptor per
// disconnect against LimitNOFILE=1024, and the process stays up as it runs out,
// so systemd never restarts it. It also strands a browser that closed cleanly
// in CLOSING, waiting for a server half it never gets, so its onclose never
// fires and it never reconnects.
func TestWebsocketClosesTheConnection(t *testing.T) {
	if _, err := os.Stat("/proc/self/fd"); err != nil {
		t.Skip("no /proc/self/fd to count descriptors with")
	}

	srv := newServer(t)
	waitClients(t, 0)

	openFDs := func() int {
		entries, err := os.ReadDir("/proc/self/fd")
		if err != nil {
			t.Fatal(err)
		}
		return len(entries)
	}

	// One round first, so anything allocated once - the dialler's idle
	// machinery - is not counted as growth.
	for i := 0; i < 5; i++ {
		conn, _, err := wsDial(t, srv, srv.URL)
		if err != nil {
			t.Fatal(err)
		}
		conn.Close()
	}
	waitClients(t, 0)

	before := openFDs()

	for i := 0; i < 30; i++ {
		conn, _, err := wsDial(t, srv, srv.URL)
		if err != nil {
			t.Fatalf("connection %d: %v", i, err)
		}
		conn.Close()
	}
	waitClients(t, 0)

	if after := openFDs(); after > before+5 {
		t.Errorf("open descriptors went %d -> %d over 30 connect/disconnect cycles", before, after)
	}
}

// TestWriterPings: a phone that leaves wifi without closing is otherwise only
// noticed by TCP keepalive, minutes later, while the page keeps showing state
// it is no longer being sent.
//
// It drives writePump directly on its own server so the interval can be short,
// rather than shortening a package variable that live connections are reading.
func TestWriterPings(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		go writePump(&wsClient{conn: conn, send: make(chan device.Event, 1)}, 20*time.Millisecond)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "ws"

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), http.Header{"Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	pinged := make(chan struct{}, 1)
	conn.SetPingHandler(func(string) error {
		select {
		case pinged <- struct{}{}:
		default:
		}
		return nil
	})

	// A ping handler only runs from inside a read; the reads also collect the
	// heartbeat frames, which are what the page itself can see.
	heartbeats := make(chan wsHeartbeat, 4)
	go func() {
		for {
			var msg wsHeartbeat
			if err := conn.ReadJSON(&msg); err != nil {
				return
			}
			select {
			case heartbeats <- msg:
			default:
			}
		}
	}()

	select {
	case <-pinged:
	case <-time.After(5 * time.Second):
		t.Fatal("no ping arrived: a dead connection would only be noticed by TCP keepalive")
	}

	select {
	case msg := <-heartbeats:
		if !msg.Heartbeat {
			t.Errorf("heartbeat frame = %+v", msg)
		}
	case <-time.After(5 * time.Second):
		t.Error("no heartbeat frame: a browser cannot see the ping, so without this the page " +
			"has no way to tell an idle socket from a dead one")
	}
}

// TestReaderDropsAClientThatStopsAnsweringPings is the other half of the
// keepalive: the ping notices, but only the read deadline reclaims. A phone
// that left wifi answers nothing, and without this its connection and its two
// goroutines sit there until TCP gives up, minutes later.
func TestReaderDropsAClientThatStopsAnsweringPings(t *testing.T) {
	waitClients(t, 0)

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &wsClient{conn: conn, send: make(chan device.Event, 1)}
		if !addClient(client) {
			conn.Close()
			return
		}
		// A ping every 20ms, and a pong must arrive within 100ms.
		go writePump(client, 20*time.Millisecond)
		go readPump(client, 100*time.Millisecond)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "ws"

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), http.Header{"Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// The client never reads, so gorilla never runs its ping handler and never
	// answers a pong - which is exactly what a sleeping phone looks like.
	waitClients(t, 1)
	waitClients(t, 0)
}

// TestHealthyClientSurvivesPings is the other half of
// TestReaderDropsAClientThatStopsAnsweringPings: the read deadline must be
// refreshed by each pong, not set once. Without the pong handler the deadline
// expires on schedule regardless of how healthy the client is, which in
// production drops every browser every wsPongTimeout - a reconnect loop on the
// connection the page depends on.
func TestHealthyClientSurvivesPings(t *testing.T) {
	waitClients(t, 0)

	const pongTimeout = 100 * time.Millisecond

	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := upgrader.Upgrade(w, r, nil)
		if err != nil {
			return
		}
		client := &wsClient{conn: conn, send: make(chan device.Event, 1)}
		if !addClient(client) {
			conn.Close()
			return
		}
		go writePump(client, pongTimeout/5)
		go readPump(client, pongTimeout)
	}))
	defer srv.Close()

	u, err := url.Parse(srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "ws"

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), http.Header{"Origin": {srv.URL}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	// Reading is what makes gorilla answer the pings.
	go func() {
		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				return
			}
		}
	}()

	waitClients(t, 1)

	// Well past several pong timeouts: a client that answers must still be here.
	deadline := time.Now().Add(pongTimeout * 8)
	for time.Now().Before(deadline) {
		if clientCount() == 0 {
			t.Fatal("a client answering every ping was dropped: the read deadline is never refreshed")
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// TestSlowClientIsDropped: the broadcaster must never block on one unresponsive
// browser. It is the only reader of device.Events(), and a write that blocks
// stops it draining, which backs up into the device layer and the lock every
// page load needs.
//
// Driven with a synthetic event channel rather than through the broker: the
// device layer drops events of its own once its queue is full, so flooding the
// broker never delivers enough here to fill a socket.
func TestSlowClientIsDropped(t *testing.T) {
	shutdown = make(chan interface{})
	events := make(chan device.Event)

	wg.Add(1)
	go broadcast(events)
	defer func() {
		close(shutdown)
		// Bounded: a broadcaster that is blocked - the thing this test exists
		// to catch - never reaches its select, so an unbounded Wait here turns
		// the assertion below into a ten-minute timeout on the whole binary.
		drained := make(chan struct{})
		go func() { wg.Wait(); close(drained) }()
		select {
		case <-drained:
		case <-time.After(5 * time.Second):
			t.Error("the broadcaster did not exit")
		}
	}()

	srv := newServer(t)
	waitClients(t, 0)

	// Connected, and then never read from - a phone whose screen went off.
	conn, _, err := wsDial(t, srv, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitClients(t, 1)

	// Push until the client is dropped: first its queue fills, then the socket
	// stops accepting, then the broadcaster takes the drop path. Every send
	// here must return promptly - that is the property under test.
	// The deadline is only a backstop. What distinguishes the broadcaster's drop
	// from the write deadline that would eventually reach the same end state is
	// the counter, checked below - not how long it took, which under -race on
	// one core overlaps the deadline it would have to exclude.
	fellBehind := wsFellBehind.Load()

	deadline := time.Now().Add(20 * time.Second)
	for i := 0; clientCountWithin(t, 5*time.Second) > 0; i++ {
		if time.Now().After(deadline) {
			t.Fatal("the client was never dropped")
		}

		sent := make(chan struct{})
		go func() {
			events <- device.Event{DeviceId: "lamp1", Payload: device.EventPayloadSwitch{State: i%2 == 0}}
			close(sent)
		}()

		select {
		case <-sent:
		case <-time.After(5 * time.Second):
			t.Fatal("the broadcaster blocked on a client that stopped reading")
		}
	}

	if wsFellBehind.Load() == fellBehind {
		t.Error("the client was dropped by the write deadline, not by the broadcaster: " +
			"the queue-full path never fired")
	}

	// Dropping the client has to close the socket too. This is the one path
	// where nothing else will: the connection is still perfectly healthy at the
	// TCP level - the browser has simply stopped reading - so the reader
	// goroutine would sit in ReadMessage holding the descriptor indefinitely.
	// That is a file descriptor per dropped client against LimitNOFILE=1024,
	// with the process staying up as it runs out.
	_ = conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		_, _, err := conn.ReadMessage()
		if err == nil {
			continue
		}
		// A timeout means the server never hung up; anything else means it did.
		// gorilla does not wrap the net error, so this checks the interface
		// rather than errors.Is.
		var netErr net.Error
		if errors.As(err, &netErr) && netErr.Timeout() {
			t.Error("the dropped client's connection was left open")
		}
		return
	}
}

// TestBroadcasterSurvivesNoDevices: device.Events() closes immediately when
// nothing is registered, which used to end the broadcaster during startup -
// logging "shutdown complete" as the server came up, and leaving nobody to
// drain the clients at the real shutdown.
func TestBroadcasterSurvivesNoDevices(t *testing.T) {
	shutdown = make(chan interface{})

	closed := make(chan device.Event)
	close(closed)

	wg.Add(1)
	go broadcast(closed)

	srv := newServer(t)
	waitClients(t, 0)

	conn, _, err := wsDial(t, srv, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()
	waitClients(t, 1)

	// Still running: the drain happens at shutdown, not at startup.
	close(shutdown)
	wg.Wait()

	waitClients(t, 0)
}

// TestStartWiresEverythingTogether covers the composition root. Every other
// test in this file assembles the handler stack by hand, so the function that
// assembles it in production was the one place none of them looked: the host
// check could be dropped from it entirely, http.allowed-hosts could stop
// reaching it, and the event fan-out could be removed, all without a single
// failure.
func TestStartWiresEverythingTogether(t *testing.T) {
	waitClients(t, 0)

	if err := Start(config.Http{
		ListenAddress: "127.0.0.1:0",
		AllowedHosts:  []string{"hal.example.com"},
	}); err != nil {
		t.Fatal(err)
	}
	defer Shutdown()

	base := "http://" + listenAddr

	request := func(host string) int {
		req, err := http.NewRequest("GET", base+"/api/state", nil)
		if err != nil {
			t.Fatal(err)
		}
		if host != "" {
			req.Host = host
		}
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatalf("GET /api/state with Host %q: %v", host, err)
		}
		defer resp.Body.Close()
		return resp.StatusCode
	}

	// The host check is wired in...
	if got := request("rebind.attacker.test"); got != http.StatusMisdirectedRequest {
		t.Errorf("a foreign Host got %d, want 421: the rebinding guard is not wired into Start", got)
	}
	// ...and it is reading the configured allowlist, not an empty one.
	if got := request("hal.example.com"); got != http.StatusOK {
		t.Errorf("the allow-listed Host got %d, want 200: http.allowed-hosts does not reach hostCheck", got)
	}
	if got := request(""); got != http.StatusOK {
		t.Errorf("an IP Host got %d, want 200", got)
	}

	// And a device event reaches a browser through the fan-out Start began.
	u, err := url.Parse(base)
	if err != nil {
		t.Fatal(err)
	}
	u.Scheme = "ws"
	u.Path = "/api/ws"

	conn, _, err := websocket.DefaultDialer.Dial(u.String(), http.Header{"Origin": {base}})
	if err != nil {
		t.Fatalf("websocket: %v", err)
	}
	defer conn.Close()
	waitClients(t, 1)

	testBroker.Deliver("stat/lamp1/POWER", "ON")

	conn.SetReadDeadline(time.Now().Add(10 * time.Second))
	for {
		var event device.Event
		if err := conn.ReadJSON(&event); err != nil {
			t.Fatalf("no event reached the browser: Start did not begin the fan-out: %v", err)
		}
		if event.DeviceId == "" {
			continue // a heartbeat
		}
		if event.DeviceId != "lamp1" {
			t.Errorf("DeviceId = %q, want lamp1", event.DeviceId)
		}
		break
	}
}
