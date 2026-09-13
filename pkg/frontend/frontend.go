package frontend

import (
	"context"
	"crypto/sha256"
	"embed"
	"fmt"
	"html/template"
	"io"
	"io/fs"
	"log"
	"mime"
	"net/http"
	"path"
	"sort"
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

var (
	srv      *http.Server
	shutdown chan interface{}
	wg       sync.WaitGroup

	indexTemplate = template.Must(template.ParseFS(templateFS, "template/index.html"))
	staticFiles   = must(fs.Sub(staticFS, "static"))
	staticETags   = buildStaticETags()
)

// buildStaticETags derives an ETag from the content of every embedded static asset. embed.FS
// reports a zero ModTime, which makes http.FileServer omit Last-Modified and therefore skip
// conditional requests altogether; without a validator of our own every page load would
// re-transfer all assets. Hashing the content rather than stamping a build time also keeps the
// validator honest across upgrades: the ETag changes exactly when the bytes do.
func init() {
	// Go's built-in table has no entry for .woff2 and otherwise falls back to
	// /etc/mime.types, which comes from a package (media-types) that need not be
	// installed on the target. Register it so the fonts are not served as
	// application/octet-stream on a minimal host.
	if err := mime.AddExtensionType(".woff2", "font/woff2"); err != nil {
		panic(err)
	}
}

func buildStaticETags() map[string]string {
	etags := make(map[string]string)
	err := fs.WalkDir(staticFiles, ".", func(p string, d fs.DirEntry, err error) error {
		if err != nil || d.IsDir() {
			return err
		}
		content, err := fs.ReadFile(staticFiles, p)
		if err != nil {
			return err
		}
		etags["/"+p] = fmt.Sprintf(`"%x"`, sha256.Sum256(content))
		return nil
	})
	if err != nil {
		panic(err)
	}
	return etags
}

// staticHandler serves the embedded static assets. Each response carries a content-derived ETag
// so that http.ServeContent can answer a revalidating browser with 304 Not Modified. The assets
// are served under fixed, unversioned URLs, so they are marked no-cache (cache, but revalidate)
// rather than immutable: a new build must not be shadowed by a stale copy of hal.css.
func staticHandler() http.Handler {
	fileServer := http.FileServer(http.FS(staticFiles))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if etag, ok := staticETags[path.Clean("/"+r.URL.Path)]; ok {
			w.Header().Set("ETag", etag)
			w.Header().Set("Cache-Control", "public, no-cache")
		}
		fileServer.ServeHTTP(w, r)
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
					err := c.WriteJSON(event)
					if err != nil {
						log.Printf("WS %v: WriteJSON: %v", c.RemoteAddr(), err)
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
	Rooms []frontendRoom
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

	w.WriteHeader(200)
	err := indexTemplate.Execute(w, &homePage{
		Rooms: frontendRooms,
	})

	if err != nil {
		log.Printf("unable to execute template: %v", err)
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
		log.Printf("device %s is not a switch", dev)
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	log.Printf("Device: %s, Target state: %v\n", switchDev, status)
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
				return
			}
		}
	}()

	log.Printf("New WS: %v", conn.RemoteAddr())

	wsConnectionsMu.Lock()
	defer wsConnectionsMu.Unlock()

	wsConnections[conn] = true
}
