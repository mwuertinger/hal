package frontend

import (
	"context"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"time"

	"github.com/gorilla/mux"
	"github.com/mwuertinger/hau/pkg/config"
	"github.com/mwuertinger/hau/pkg/device"
	"github.com/mwuertinger/hau/pkg/mqtt"
	"github.com/pkg/errors"
)

var (
	srv         *http.Server
	mqttService mqtt.Broker
)

// Start starts the HTTP server listening on listenAddress in the format address:port. The function returns immediately
// and calls log.Fatal() should an error occur.
func Start(httpConfig config.Http, mqttService mqtt.Broker) error {
	if srv != nil {
		return errors.New("already started")
	}

	r := mux.NewRouter()
	r.HandleFunc("/", homeHandler).Methods("GET")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	r.HandleFunc("/api/{device}", switchHandler).Methods("PUT")

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
func Shutdown() error {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	return srv.Shutdown(ctx)
}

type frontendDevice struct {
	ID   string
	Name string
}

type homePage struct {
	Devices []frontendDevice
}

func homeHandler(w http.ResponseWriter, r *http.Request) {
	tmplSrc, err := ioutil.ReadFile("template/index.html")
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
	frontendDevices := make([]frontendDevice, len(devices), len(devices))
	for i, d := range devices {
		log.Printf("device=%v", d)
		frontendDevices[i] = frontendDevice{
			ID:   d.ID(),
			Name: d.Name(),
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

	log.Printf("Device: %s, Target state: %b\n", switchDev, status)
	if err = switchDev.Switch(status); err != nil {
		log.Printf("send command failed: %v", err)
		w.WriteHeader(http.StatusInternalServerError)
		return
	}

	w.WriteHeader(http.StatusOK)
}
