package frontend

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
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

	conn, _, err := wsDial(t, srv, srv.URL)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.Close()

	waitClients(t, 1)

	testBroker.Deliver("stat/lamp1/POWER", "ON")

	conn.SetReadDeadline(time.Now().Add(5 * time.Second))
	var event device.Event
	if err := conn.ReadJSON(&event); err != nil {
		t.Fatalf("no event reached the browser: %v", err)
	}
	if event.DeviceId != "lamp1" {
		t.Errorf("DeviceId = %q, want lamp1", event.DeviceId)
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
