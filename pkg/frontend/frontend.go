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
	"github.com/mwuertinger/hau/pkg/mqtt"
	"github.com/pkg/errors"
)

var (
	srv         *http.Server
	mqttService mqtt.Service
)

// Start starts the HTTP server listening on listenAddress in the format address:port. The function returns immediately
// and calls log.Fatal() should an error occur.
func Start(httpConfig config.Http, mqttService mqtt.Service) error {
	if srv != nil {
		return errors.New("already started")
	}

	r := mux.NewRouter()
	r.HandleFunc("/", homeHandler).Methods("GET")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	r.HandleFunc("/api/{device}", socketHandler).Methods("PUT")

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

type lamp struct {
	Name   string
	Device string
}

type homePage struct {
	Lamps []lamp
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

	w.WriteHeader(200)
	err = tmpl.Execute(w, &homePage{
		Lamps: []lamp{
			{"Stehlampe", "socket01"},
		},
	})

	if err != nil {
		log.Printf("unable to execute template: %v", err)
	}
}

func socketHandler(w http.ResponseWriter, r *http.Request) {
	vars := mux.Vars(r)

	body, err := ioutil.ReadAll(r.Body)
	if err != nil {
		log.Printf("reading body failed: %v", err)
		w.WriteHeader(500)
		return
	}

	device := vars["device"]
	var status bool
	switch string(body) {
	case "on":
		status = true
	case "off":
		status = false
	default:
		log.Printf("invalid status: %s", string(body))
		w.WriteHeader(500)
		return
	}

	log.Printf("Device: %s, Action: %s\n", device, string(body))
	if err = mqttService.SendCommand(device, status); err != nil {
		log.Printf("send command failed: %v", err)
		w.WriteHeader(500)
		return
	}

	w.WriteHeader(http.StatusOK)
}
