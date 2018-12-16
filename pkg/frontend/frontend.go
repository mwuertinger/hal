package frontend

import (
	"context"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/gorilla/mux"
	"github.com/gorilla/websocket"
	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/device"
	"github.com/pkg/errors"
)

const (
	AllId = "all" // pseudo-device ID representing all devices
)

var (
	srv      *http.Server
	shutdown chan interface{}
	wg       sync.WaitGroup
)

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
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("frontend/static"))))
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

type frontendDevice struct {
	ID    string
	Name  string
	State string
}

type homePage struct {
	Devices []frontendDevice
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	tmplSrc, err := ioutil.ReadFile("frontend/template/index.html")
	if err != nil {
		log.Printf("unable to read template: %v", err)
		w.WriteHeader(500)
		return
	}

	tmpl, err := template.New("index.html").Parse(string(tmplSrc))
	if err != nil {
		log.Printf("unable to parse template: %v", err)
		w.WriteHeader(500)
		return
	}

	devices := device.List()
	frontendDevices := make([]frontendDevice, len(devices)+1, len(devices)+1)
	frontendDevices[0] = frontendDevice{
		ID:    "all",
		Name:  "All",
		State: "",
	}

	for i, d := range devices {
		state := ""
		devSwitch, ok := d.(device.Switch)
		if ok {
			if devSwitch.LastKnownState() {
				state = "Device is on"
			} else {
				state = "Device is off"
			}
		}

		frontendDevices[i+1] = frontendDevice{
			ID:    d.ID(),
			Name:  d.Name(),
			State: state,
		}
	}

	w.WriteHeader(200)
	err = tmpl.Execute(w, &homePage{
		Devices: frontendDevices,
	})

	if err != nil {
		log.Printf("unable to execute template: %v", err)
	}
}

func switchHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("reading body failed: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	var status bool
	switch string(body) {
	case "on":
		status = true
	case "off":
		status = false
	default:
		log.Printf("invalid status: %s", string(body))
		w.WriteHeader(http.StatusBadRequest)
		return
	}

	deviceId := vars["device"]
	if deviceId == AllId {
		isErr := false
		for _, dev := range device.List() {
			switchDev, success := dev.(device.Switch)
			if !success {
				continue
			}
			if err := switchDev.Switch(status); err != nil {
				log.Printf("send command failed: %v", err)
				isErr = true
			}
		}
		if isErr {
			w.WriteHeader(http.StatusInternalServerError)
		}
	} else {
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
