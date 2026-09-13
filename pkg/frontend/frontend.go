package frontend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"net"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/websocket"
	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/device"
)

//go:embed template/index.html
var templateFS embed.FS

// static/ holds the whole frontend: hand-written CSS and JS, no vendored framework.
// Nothing here is fetched from a CDN at runtime either, because hal.service confines
// outbound traffic to the LAN.
//
//go:embed static
var staticFS embed.FS

// wsWriteTimeout bounds a single frame written to one websocket client. It only
// has to be generous enough for a healthy client on a slow link; a client that
// cannot absorb one small JSON event within it is treated as gone.
const wsWriteTimeout = 5 * time.Second

var (
	// lifecycleMu makes Start and Shutdown safe to call in any order and more
	// than once, so an early-exit path that shuts down before startup finished
	// cannot turn a clean exit into a panic.
	lifecycleMu sync.Mutex
	srv         *http.Server
	shutdown    chan interface{}
	wg          sync.WaitGroup

	staticFiles = must(fs.Sub(staticFS, "static"))
	assets      = buildAssets()

	indexTemplate = template.Must(template.New("index.html").
			Funcs(template.FuncMap{"asset": assetURL}).
			ParseFS(templateFS, "template/index.html"))
)

func init() {
	// A typo in an asset name only shows up when Execute aborts part-way through,
	// which reaches the user as a blank page with a 200 and nothing but a log line
	// to say so. Rendering both branches here turns that into a refusal to start.
	for _, page := range []homePage{
		{},
		{Rooms: []frontendRoom{{
			Name:    "room",
			Devices: []frontendDevice{{ID: "off", Name: "off"}, {ID: "on", Name: "on", State: true}},
			OnCount: 1,
			AnyOn:   true,
		}}, OnCount: 1, Total: 2},
	} {
		if err := indexTemplate.Execute(io.Discard, &page); err != nil {
			panic(err)
		}
	}
}

// contentTypes pins the media types HAL actually ships, rather than asking the
// mime package for them.
//
// Go's builtin table has no entry for .woff2 or .txt, so it would fall back to
// /etc/mime.types - a file from a package that need not be installed on the
// target. Registering the missing types with mime.AddExtensionType is not a fix
// either, because every package-level variable is initialised before any init()
// runs, so the registration would land after buildAssets had already read the
// table. Pinning them here has no ordering to get wrong.
//
// Getting this wrong is quiet but not harmless: an empty Content-Type is worse
// than a missing one, because http.ServeContent treats it as already set and
// skips sniffing, so the response goes out with no type at all.
//
// There is deliberately no fallback to mime.TypeByExtension. Falling back would
// put the host dependence straight back: .ico, .woff and .ttf resolve on a
// developer machine only because /etc/mime.types is there, and .otf resolves
// through it to an OpenDocument formula-template type, which is simply wrong.
// An asset would then pass every check here and either panic or be mistyped on
// the Pi. Anything not listed panics at startup instead, on every host alike,
// which a test catches long before a deploy does.
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".txt":   "text/plain; charset=utf-8",
	".woff2": "font/woff2",
}

// staticAsset is one embedded file as it is actually served.
type staticAsset struct {
	// publicPath contains a hash of the content, e.g. /static/css/hal.1a2b3c.css.
	publicPath  string
	contentType string
	content     []byte
	etag        string
}

// buildAssets gives every static file a URL derived from its content.
//
// Validators alone are not enough. Before these assets were embedded they were
// served by http.FileServer(http.Dir(...)), which sends Last-Modified and no
// Cache-Control, and a response like that is heuristically cacheable: RFC 9111
// lets a client treat it as fresh for a fraction of its age - Firefox uses 10%,
// so a file with a year-old mtime stays fresh for over a month - and for that
// whole window the client never revalidates, so it never sees a new ETag either.
// A browser that cached hal.css from the old deployment therefore went on
// rendering the old stylesheet over the new markup.
//
// Hashing the URL fixes both directions: a new build is a URL no cache has ever
// seen, so a poisoned cache heals on its own, and an unchanged build keeps its
// URL, so the response can be marked immutable and skip revalidation entirely.
func buildAssets() map[string]*staticAsset {
	out, err := buildAssetsFS(staticFiles)
	if err != nil {
		panic(err)
	}
	return out
}

// buildAssetsFS is the fallible half, split out so its rejections can be tested.
// buildAssets runs during package-variable initialisation, so anything it panics
// on takes the test binary down before a single Test function runs - which makes
// an assertion about a rejection unreachable if it can only be written against
// the embedded FS.
func buildAssetsFS(fsys fs.FS) (map[string]*staticAsset, error) {
	var paths []string
	err := fs.WalkDir(fsys, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		return nil, err
	}

	// A logical path that is a prefix of another corrupts the longer one's
	// rewritten URL, and sorting longest-first does not save it: hashing
	// css/hal.css.map yields /static/css/hal.css.<hash>.map, which still
	// contains /static/css/hal.css, so the second substitution eats it. The
	// affected shape is one path being another plus a suffix - hal.css against
	// hal.css.map or hal.css.gz - and not, as it looks, any shared stem:
	// inter-latin.woff against inter-latin.woff2 is fine, because there the hash
	// lands ahead of the single extension.
	for _, a := range paths {
		for _, b := range paths {
			if a != b && strings.HasPrefix(b, a) {
				return nil, fmt.Errorf("static asset %q is a prefix of %q: rewriting url() references would corrupt the longer one", a, b)
			}
		}
	}

	// Stylesheets last: they refer to other assets by URL, so those must already
	// have been hashed before a stylesheet's own content is final.
	sort.Slice(paths, func(i, j int) bool {
		iCSS, jCSS := path.Ext(paths[i]) == ".css", path.Ext(paths[j]) == ".css"
		if iCSS != jCSS {
			return jCSS
		}
		return paths[i] < paths[j]
	})

	out := make(map[string]*staticAsset, len(paths))
	for _, p := range paths {
		content, err := fs.ReadFile(fsys, p)
		if err != nil {
			return nil, err
		}
		ext := path.Ext(p)

		if ext == ".css" {
			content = rewriteAssetRefs(content, out)
		} else if bytes.Contains(content, []byte("/static/")) {
			// Only stylesheets get their references rewritten, so anything else
			// naming an asset would silently keep an unhashed URL and 404.
			return nil, fmt.Errorf("static asset %q references /static/ but is not a stylesheet, so its URLs are never rewritten", p)
		}

		contentType, ok := contentTypes[ext]
		if !ok {
			return nil, fmt.Errorf("no content type pinned for %q; add %q to contentTypes", p, ext)
		}

		// The content type is hashed along with the bytes. It is served under
		// immutable for a year, so changing the type of an otherwise unchanged
		// asset without changing its URL would leave every client that had
		// already visited stuck with the old one - and browsers refuse a
		// stylesheet served as the wrong type outright.
		sum := sha256.Sum256(append([]byte(contentType+"\x00"), content...))
		hash := hex.EncodeToString(sum[:])[:16]

		out[p] = &staticAsset{
			publicPath:  "/static/" + strings.TrimSuffix(p, ext) + "." + hash + ext,
			contentType: contentType,
			content:     content,
			etag:        `"` + hash + `"`,
		}
	}
	return out, nil
}

// rewriteAssetRefs points a stylesheet's url() references at the hashed paths of
// the assets it names, so a font is cache-busted by the same mechanism as the
// stylesheet that loads it.
func rewriteAssetRefs(content []byte, built map[string]*staticAsset) []byte {
	logicals := make([]string, 0, len(built))
	for logical := range built {
		logicals = append(logicals, logical)
	}
	// Longest first, so the rewrite does not depend on map iteration order.
	// buildAssetsFS has already rejected the case this cannot handle - a path
	// that is another plus a suffix - because longest-first does not save that
	// one; see the check there.
	sort.Slice(logicals, func(i, j int) bool { return len(logicals[i]) > len(logicals[j]) })

	for _, logical := range logicals {
		content = bytes.ReplaceAll(content, []byte("/static/"+logical), []byte(built[logical].publicPath))
	}
	return content
}

// assetURL is the template's "asset" function: it maps a path inside static/ to
// the hashed URL that path is served at.
func assetURL(logical string) (string, error) {
	a, ok := assets[logical]
	if !ok {
		return "", fmt.Errorf("unknown static asset %q", logical)
	}
	return a.publicPath, nil
}

// staticHandler serves the embedded assets from their content-hashed URLs. Because
// the URL changes whenever the bytes do, a cached copy can never be the wrong one -
// which is what makes immutable safe, and immutable is what spares a phone a
// revalidation round trip per asset on every page load.
func staticHandler() http.Handler {
	byPath := make(map[string]*staticAsset, len(assets))
	for _, a := range assets {
		byPath[a.publicPath] = a
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// StripPrefix has already removed "/static/", trailing slash included.
		a, ok := byPath[path.Clean("/static/"+r.URL.Path)]
		if !ok {
			// Everything else in this handler is cacheable for a year, and a
			// 404 is heuristically cacheable too. On a URL that never changes
			// back, a cached one would be permanent.
			w.Header().Set("Cache-Control", "no-store")
			http.NotFound(w, r)
			return
		}

		w.Header().Set("Content-Type", a.contentType)
		w.Header().Set("ETag", a.etag)
		w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
		http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(a.content))
	})
}

// must unwraps a (value, error) pair, panicking if err is non-nil.
func must[T any](v T, err error) T {
	if err != nil {
		panic(err)
	}
	return v
}

// Start starts the HTTP server listening on listenAddress in the format address:port. The function returns immediately
// and calls log.Fatal() should an error occur.
func Start(httpConfig config.Http) error {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()

	if srv != nil {
		return errors.New("already started")
	}

	shutdown = make(chan interface{})
	wg.Add(1)

	go broadcast(device.Events())

	// The goroutine closes over its own reference rather than reading the
	// package variable, which Shutdown clears.
	server := &http.Server{
		Handler:      hostCheck(router(), httpConfig.AllowedHosts),
		Addr:         httpConfig.ListenAddress,
		WriteTimeout: 5 * time.Second,
		ReadTimeout:  5 * time.Second,
	}
	srv = server

	go func() {
		err := server.ListenAndServe()

		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	return nil
}

// router builds the route table. Separate from Start so that a test can serve
// it without binding a port or starting the event fan-out.
//
// The patterns are net/http's own (Go 1.22): a method in the pattern restricts
// the route to it, "GET" also matches HEAD, "{$}" pins the root to an exact
// match rather than a catch-all prefix, and a path that matches no method
// answers 405 with an Allow header. That last part is why /static/ names its
// methods too - it used to serve the file body in reply to DELETE, and a
// non-error response to an unsafe method makes a cache drop the entry that
// content-addressed URLs exist to keep.
func router() *http.ServeMux {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", homeHandler)
	mux.Handle("GET /static/", http.StripPrefix("/static/", staticHandler()))
	mux.HandleFunc("GET /api/state", stateHandler)
	mux.HandleFunc("PUT /api/{device}", switchHandler)
	mux.HandleFunc("GET /api/ws", wsHandler)
	return mux
}

// hostCheck rejects a request whose Host header names a domain rather than this
// machine.
//
// HAL has no authentication: being on the LAN is the credential. DNS rebinding
// is the standard way around that - a page on a public domain whose DNS is
// re-pointed at 192.168.x.x becomes same-origin with HAL as far as the browser
// is concerned, and can then switch lamps and read the event stream despite the
// same-origin policy and the websocket origin check. What it cannot do is
// change the Host header the browser sends, which is the name the user typed.
//
// So: IP literals, localhost, single-label names and the usual LAN suffixes are
// this machine; anything else is a domain, and is refused unless
// http.allowed-hosts names it.
func hostCheck(next http.Handler, allowed []string) http.Handler {
	permitted := make(map[string]bool, len(allowed))
	for _, name := range allowed {
		permitted[strings.ToLower(name)] = true
	}

	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !hostIsLocal(r.Host, permitted) {
			log.Printf("refused request for host %q from %v", r.Host, r.RemoteAddr)
			http.Error(w, "unrecognised Host", http.StatusMisdirectedRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// lanSuffixes are the domain suffixes reserved for, or conventionally used on,
// local networks. A name under one of them cannot be registered publicly, so it
// cannot be the vehicle for a rebinding attack.
var lanSuffixes = []string{".local", ".lan", ".home", ".home.arpa", ".internal", ".localhost"}

func hostIsLocal(host string, permitted map[string]bool) bool {
	name := host
	if h, _, err := net.SplitHostPort(host); err == nil {
		name = h
	}
	name = strings.ToLower(strings.TrimSuffix(name, "."))

	if name == "" {
		// HTTP/1.1 requires a Host header, and HTTP/2 synthesises one.
		return false
	}
	if permitted[name] {
		return true
	}
	if name == "localhost" {
		return true
	}
	// An IPv6 literal keeps its brackets when there is no port.
	if net.ParseIP(strings.Trim(name, "[]")) != nil {
		return true
	}
	if !strings.Contains(name, ".") {
		// A single-label name is not resolvable on the public internet.
		return true
	}
	for _, suffix := range lanSuffixes {
		if strings.HasSuffix(name, suffix) {
			return true
		}
	}
	return false
}

// broadcast fans device events out to every connected browser. It owns the
// only read of device.Events().
func broadcast(eventChan <-chan device.Event) {
	defer func() {
		// Closing the queue ends each writer, and each writer closes its own
		// connection - so nothing here has to wait on a socket.
		wsConnectionsMu.Lock()
		for c := range wsConnections {
			delete(wsConnections, c)
			close(c.send)
		}
		wsConnectionsMu.Unlock()
		log.Printf("frontend: shutdown complete")
		wg.Done()
	}()

	for {
		select {
		case event, ok := <-eventChan:
			if !ok {
				// Every device has stopped - which with no devices configured
				// is true from the start. Keep serving: browsers still connect,
				// and the drain above still has to run at shutdown rather than
				// during startup. A nil channel blocks forever in select.
				eventChan = nil
				continue
			}

			log.Printf("New event: %v", event)

			// Hand each client its event and move on. Writing here instead
			// would make one unresponsive client everyone's problem: this
			// goroutine is the only reader of device.Events(), and a write
			// that blocks stops it draining, which backs up into
			// processNotification while that holds the device lock - the
			// same lock homeHandler and stateHandler need. Bounding the
			// write only capped that at wsWriteTimeout per stalled client;
			// not blocking at all removes it.
			wsConnectionsMu.Lock()
			for c := range wsConnections {
				select {
				case c.send <- event:
				default:
					log.Printf("WS %v: %d events behind, dropping", c.conn.RemoteAddr(), wsSendQueue)
					delete(wsConnections, c)
					close(c.send)
				}
			}
			wsConnectionsMu.Unlock()
		case <-shutdown:
			return
		}
	}
}

// Shutdown the server waiting at most 5 seconds for in-flight connections to terminate.
// Calling it without a successful Start, or twice, does nothing.
func Shutdown() {
	lifecycleMu.Lock()
	defer lifecycleMu.Unlock()

	if srv == nil {
		return
	}

	server := srv
	srv = nil

	close(shutdown)
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := server.Shutdown(ctx); err != nil {
		log.Printf("frontend shutdown: %v", err)
	}
}

type frontendRoom struct {
	Name    string
	Devices []frontendDevice
	// OnCount is rendered in the room header. The template could not count the
	// devices that are on by itself: text/template has no accumulator.
	OnCount int
	AnyOn   bool
}

type frontendDevice struct {
	ID    string
	Name  string
	State bool
}

type homePage struct {
	Rooms   []frontendRoom
	OnCount int
	Total   int
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	page := buildHomePage(device.List())

	// The page carries live device state, so it must never be cached - and a
	// response with neither Cache-Control nor Last-Modified cannot be cached
	// heuristically either, which is the only reason the markup stayed fresh
	// while the stylesheet went stale.
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	w.WriteHeader(200)
	err := indexTemplate.Execute(w, &page)

	if err != nil {
		log.Printf("unable to execute template: %v", err)
	}
}

// buildHomePage groups devices into rooms for the template.
func buildHomePage(devices []device.Device) homePage {
	rooms := make(map[string]*frontendRoom)
	for _, d := range devices {
		// Checked, like stateHandler's: the Device/Switch split exists so that
		// a device need not be switchable, and an unchecked assertion here
		// turned the first such device into a panic on every page load.
		devSwitch, ok := d.(device.Switch)
		if !ok {
			continue
		}
		if _, ok := rooms[d.Location()]; !ok {
			rooms[d.Location()] = &frontendRoom{
				Name: d.Location(),
			}
		}
		room := rooms[d.Location()]
		room.Devices = append(room.Devices, frontendDevice{
			ID:    d.ID(),
			Name:  d.Name(),
			State: devSwitch.LastKnownState(),
		})
	}

	var frontendRooms []frontendRoom
	for _, room := range rooms {
		for _, d := range room.Devices {
			if d.State {
				room.OnCount++
			}
		}
		room.AnyOn = room.OnCount > 0
		frontendRooms = append(frontendRooms, *room)
	}
	sort.Slice(frontendRooms, func(i, j int) bool {
		return frontendRooms[i].Name < frontendRooms[j].Name
	})

	page := homePage{Rooms: frontendRooms}
	for _, room := range frontendRooms {
		page.OnCount += room.OnCount
		page.Total += len(room.Devices)
	}
	return page
}

// stateHandler reports the last known state of every switch. The page renders its
// initial state from the template, but a websocket that drops loses every event for
// as long as it is down, so the client refetches this whenever it (re)connects.
func stateHandler(w http.ResponseWriter, r *http.Request) {
	states := make(map[string]bool)
	for _, d := range device.List() {
		if switchDev, ok := d.(device.Switch); ok {
			states[d.ID()] = switchDev.LastKnownState()
		}
	}

	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	if err := json.NewEncoder(w).Encode(states); err != nil {
		log.Printf("unable to encode state: %v", err)
	}
}

func switchHandler(w http.ResponseWriter, r *http.Request) {
	// The body is "true" or "false". ReadTimeout bounds how long a client may
	// take to send one, but nothing bounded how much it could send.
	r.Body = http.MaxBytesReader(w, r.Body, 64)

	body, err := io.ReadAll(r.Body)
	if err != nil {
		var toLarge *http.MaxBytesError
		if errors.As(err, &toLarge) {
			log.Printf("body over %d bytes", toLarge.Limit)
			w.WriteHeader(http.StatusRequestEntityTooLarge)
			return
		}
		log.Printf("reading body failed: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var status bool
	switch string(body) {
	case "true":
		status = true
	case "false":
		status = false
	default:
		log.Printf("invalid status: %s", string(body))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	deviceId := r.PathValue("device")
	dev := device.Get(deviceId)
	if dev == nil {
		log.Printf("device not found: %s", deviceId)
		w.WriteHeader(http.StatusNotFound)
		return
	}

	switchDev, success := dev.(device.Switch)
	if !success {
		log.Printf("device %s is not a switch", deviceId)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Printf("Device: %s, Target state: %v", deviceId, status)
	if err = switchDev.Switch(status); err != nil {
		log.Printf("send command failed: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}

// wsSendQueue is how far one client may fall behind before it is dropped.
// Generous next to the handful of events a lamp produces, and small enough that
// a client that has genuinely stopped reading is recognised quickly.
const wsSendQueue = 64

const (
	// wsMaxClients caps concurrent websockets. A household has a handful of
	// phones; without a ceiling, each connection costs an fd, two goroutines
	// and two buffers until LimitNOFILE=1024 is reached, at which point the
	// whole HTTP server stops accepting - and the process stays up, so systemd
	// never restarts it.
	wsMaxClients = 32

	// wsReadLimit bounds one inbound frame. The page never sends anything, so
	// this only has to be large enough for a close frame. Gorilla treats the
	// default of 0 as unlimited, and ReadMessage buffers a whole message before
	// it can be discarded, so a single client frame could otherwise be sized to
	// exhaust the Pi's memory.
	wsReadLimit = 512

	// wsPingInterval and wsPongTimeout detect a connection that died without a
	// close frame - a phone that walked out of wifi, a router that dropped the
	// NAT entry. Without them the only backstop is TCP keepalive, and the
	// browser meanwhile keeps showing state it is no longer being sent.
	wsPingInterval = 30 * time.Second
	wsPongTimeout  = 90 * time.Second
)

// upgrader replaces the package-level websocket.Upgrade, which is deprecated
// and, more to the point, sets CheckOrigin to accept everything. Websockets are
// exempt from the same-origin policy and from CORS preflight, so that let any
// page the user happened to visit open this socket and read the event stream -
// every lamp transition in the house, timestamped. The zero value's CheckOrigin
// is a same-origin check, which is what this needs.
var upgrader = websocket.Upgrader{ReadBufferSize: 1024, WriteBufferSize: 1024}

// wsClient is one browser: its connection plus the queue feeding it. Only the
// client's own writer goroutine touches the connection for writing, which is
// what gorilla requires.
type wsClient struct {
	conn *websocket.Conn
	send chan device.Event
}

var (
	// Allocated here rather than in Start, which assigned it without holding
	// the mutex that guards every other access - harmless while Start ran
	// exactly once before the first connection, and a data race the moment it
	// did not. Shutdown empties the map, so a restart starts clean anyway.
	wsConnections   = make(map[*wsClient]bool)
	wsConnectionsMu sync.Mutex
)

// dropClient removes a client and closes its queue, which is what ends its
// writer. Closing the queue is guarded by the client still being registered, so
// it happens exactly once however many goroutines notice the failure.
func dropClient(c *wsClient) {
	wsConnectionsMu.Lock()
	defer wsConnectionsMu.Unlock()
	if wsConnections[c] {
		delete(wsConnections, c)
		close(c.send)
	}
}

// addClient registers a client unless the ceiling is already reached.
func addClient(c *wsClient) bool {
	wsConnectionsMu.Lock()
	defer wsConnectionsMu.Unlock()
	if len(wsConnections) >= wsMaxClients {
		return false
	}
	wsConnections[c] = true
	return true
}

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := upgrader.Upgrade(w, r, nil)
	if err != nil {
		// Upgrader has already written the response - 400 for a bad handshake,
		// 403 for a foreign origin. Writing another status here would both
		// override the accurate one and, where the hijack already happened,
		// log a write on a hijacked connection.
		log.Printf("websocket upgrade from %v: %v", r.RemoteAddr, err)
		return
	}

	conn.SetReadLimit(wsReadLimit)

	client := &wsClient{conn: conn, send: make(chan device.Event, wsSendQueue)}

	// Register before starting either goroutine. The other order leaves a window
	// where a connection that dies immediately is not yet in the map, so the
	// reader's dropClient finds nothing to do, the queue is never closed, and the
	// writer parks on the range for the life of the process. A broadcast landing
	// in this window instead either buffers or takes the drop path, and a writer
	// that then starts on an already-closed queue drains it and exits.
	if !addClient(client) {
		log.Printf("WS %v: refused, %d clients already connected", conn.RemoteAddr(), wsMaxClients)
		_ = conn.WriteControl(websocket.CloseMessage,
			websocket.FormatCloseMessage(websocket.CloseTryAgainLater, "too many clients"),
			time.Now().Add(time.Second))
		conn.Close()
		return
	}

	log.Printf("New WS: %v", conn.RemoteAddr())

	// Writer. Ends when the queue is closed, or when a write fails - a stalled
	// client hits the deadline rather than blocking here forever. It also owns
	// the ping ticker, because gorilla allows only one writer at a time.
	go func() {
		ticker := time.NewTicker(wsPingInterval)
		defer ticker.Stop()

		for {
			select {
			case event, ok := <-client.send:
				if !ok {
					dropClient(client)
					conn.Close()
					return
				}
				if err := conn.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
					log.Printf("WS %v: SetWriteDeadline: %v", conn.RemoteAddr(), err)
				}
				if err := conn.WriteJSON(event); err != nil {
					log.Printf("WS %v: WriteJSON: %v, dropping", conn.RemoteAddr(), err)
					dropClient(client)
					// Nothing else closes the hijacked connection. Leaving it
					// open leaks a file descriptor per disconnect - against
					// LimitNOFILE=1024 in hal.service - and strands a client
					// that closed cleanly in CLOSING, still waiting for the
					// server half of the handshake, so its onclose never fires
					// and it never reconnects.
					conn.Close()
					return
				}
			case <-ticker.C:
				if err := conn.WriteControl(websocket.PingMessage, nil, time.Now().Add(wsWriteTimeout)); err != nil {
					log.Printf("WS %v: ping: %v, dropping", conn.RemoteAddr(), err)
					dropClient(client)
					conn.Close()
					return
				}
			}
		}
	}()

	// Reader. The page never sends anything, so this exists to notice the
	// connection going away - either by a read error, or by the pong for one of
	// the writer's pings failing to arrive within wsPongTimeout.
	go func() {
		_ = conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		conn.SetPongHandler(func(string) error {
			return conn.SetReadDeadline(time.Now().Add(wsPongTimeout))
		})

		for {
			if _, _, err := conn.ReadMessage(); err != nil {
				log.Printf("ReadMessage() error: %v, Removing WS: %v", err, conn.RemoteAddr())
				dropClient(client)
				conn.Close()
				return
			}
		}
	}()
}
