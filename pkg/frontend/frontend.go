package frontend

import (
	"bytes"
	"context"
	"crypto/sha256"
	"embed"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"sort"
	"strings"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/device"
	"github.com/pkg/errors"
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
	srv      *http.Server
	shutdown chan interface{}
	wg       sync.WaitGroup

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
		{Rooms: []frontendRoom{{Name: "room", Devices: []frontendDevice{{ID: "d", Name: "n"}}}}, Total: 1},
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
var contentTypes = map[string]string{
	".css":   "text/css; charset=utf-8",
	".js":    "text/javascript; charset=utf-8",
	".svg":   "image/svg+xml",
	".txt":   "text/plain; charset=utf-8",
	".woff2": "font/woff2",
}

func contentTypeFor(ext string) string {
	if typ, ok := contentTypes[ext]; ok {
		return typ
	}
	return mime.TypeByExtension(ext)
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
	var paths []string
	err := fs.WalkDir(staticFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		paths = append(paths, p)
		return nil
	})
	if err != nil {
		panic(err)
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

	for _, a := range paths {
		for _, b := range paths {
			if a != b && strings.HasPrefix(b, a) {
				panic(fmt.Sprintf("static asset %q is a prefix of %q: rewriting url() references would corrupt the longer one", a, b))
			}
		}
	}

	out := make(map[string]*staticAsset, len(paths))
	for _, p := range paths {
		content := must(fs.ReadFile(staticFiles, p))
		if path.Ext(p) == ".css" {
			content = rewriteAssetRefs(content, out)
		}

		// Only stylesheets get their references rewritten, so anything else that
		// names an asset would silently keep an unhashed URL and 404.
		if path.Ext(p) != ".css" && bytes.Contains(content, []byte("/static/")) {
			panic(fmt.Sprintf("static asset %q references /static/ but is not a stylesheet, so its URLs are never rewritten", p))
		}

		sum := sha256.Sum256(content)
		hash := hex.EncodeToString(sum[:])[:16]
		ext := path.Ext(p)

		contentType := contentTypeFor(ext)
		if contentType == "" {
			panic(fmt.Sprintf("no content type for %q; add its extension to contentTypes", p))
		}

		out[p] = &staticAsset{
			publicPath:  "/static/" + strings.TrimSuffix(p, ext) + "." + hash + ext,
			contentType: contentType,
			content:     content,
			etag:        `"` + hash + `"`,
		}
	}
	return out
}

// rewriteAssetRefs points a stylesheet's url() references at the hashed paths of
// the assets it names, so a font is cache-busted by the same mechanism as the
// stylesheet that loads it.
func rewriteAssetRefs(content []byte, built map[string]*staticAsset) []byte {
	logicals := make([]string, 0, len(built))
	for logical := range built {
		logicals = append(logicals, logical)
	}
	// Longest first, so a path that is a prefix of another cannot be substituted
	// inside it. buildAssets rejects that case outright; this keeps the rewrite
	// order-independent regardless.
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
	if srv != nil {
		return errors.New("already started")
	}

	wsConnections = make(map[*websocket.Conn]bool)

	shutdown = make(chan interface{})
	wg.Add(1)

	go func() {
		eventChan := device.Events()

		for {
			select {
			case event, ok := <-eventChan:
				if !ok {
					goto shutdown
				}

				log.Printf("New event: %v", event)

				wsConnectionsMu.Lock()
				for c := range wsConnections {
					// Bound every write. A suspended phone sends no RST, so
					// without this a single unresponsive client makes the whole
					// dashboard unresponsive for everyone until the kernel gives
					// up on the connection minutes later.
					if err := c.SetWriteDeadline(time.Now().Add(wsWriteTimeout)); err != nil {
						log.Printf("WS %v: SetWriteDeadline: %v", c.RemoteAddr(), err)
					}
					if err := c.WriteJSON(event); err != nil {
						log.Printf("WS %v: WriteJSON: %v, dropping", c.RemoteAddr(), err)
						delete(wsConnections, c)
						c.Close()
					}
				}
				wsConnectionsMu.Unlock()
			case <-shutdown:
				goto shutdown
			}
		}

	shutdown:
		wsConnectionsMu.Lock()
		for c := range wsConnections {
			c.Close()
		}
		wsConnectionsMu.Unlock()
		log.Printf("frontend: shutdown complete")
		wg.Done()
	}()

	r := mux.NewRouter()
	r.HandleFunc("/", homeHandler).Methods("GET")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", staticHandler()))
	r.HandleFunc("/api/state", stateHandler).Methods("GET")
	r.HandleFunc("/api/{device}", switchHandler).Methods("PUT")
	r.HandleFunc("/api/ws", wsHandler)

	srv = &http.Server{
		Handler:      r,
		Addr:         httpConfig.ListenAddress,
		WriteTimeout: 5 * time.Second,
		ReadTimeout:  5 * time.Second,
	}

	go func() {
		err := srv.ListenAndServe()

		if err != nil && err != http.ErrServerClosed {
			log.Fatalf("HTTP server: %v", err)
		}
	}()

	return nil
}

// Shutdown the server waiting at most 5 seconds for in-flight connections to terminate.
func Shutdown() {
	close(shutdown)
	wg.Wait()

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
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
	rooms := make(map[string]*frontendRoom)
	for _, d := range device.List() {
		if _, ok := rooms[d.Location()]; !ok {
			rooms[d.Location()] = &frontendRoom{
				Name: d.Location(),
			}
		}
		room := rooms[d.Location()]
		devSwitch := d.(device.Switch)
		room.Devices = append(rooms[d.Location()].Devices, frontendDevice{
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
	vars := mux.Vars(r)

	body, err := io.ReadAll(r.Body)
	if err != nil {
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

	deviceId := vars["device"]
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

var (
	wsConnections   map[*websocket.Conn]bool
	wsConnectionsMu sync.RWMutex
)

func wsHandler(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Upgrade(w, r, nil, 1024, 1024)
	if err != nil {
		log.Printf("websocket.Upgrade: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	go func() {
		for {
			_, _, err := conn.ReadMessage()
			if err != nil {
				log.Printf("ReadMessage() error: %v, Removing WS: %v", err, conn.RemoteAddr())
				wsConnectionsMu.Lock()
				delete(wsConnections, conn)
				wsConnectionsMu.Unlock()

				// Nothing else closes the hijacked connection. Leaving it open
				// leaks a file descriptor per disconnect - against LimitNOFILE=1024
				// in hal.service - and strands a client that closed cleanly in
				// CLOSING, still waiting for the server half of the handshake, so
				// its onclose never fires and it never reconnects.
				conn.Close()
				return
			}
		}
	}()

	log.Printf("New WS: %v", conn.RemoteAddr())

	wsConnectionsMu.Lock()
	defer wsConnectionsMu.Unlock()

	wsConnections[conn] = true
}
