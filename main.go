package main

import (
	"crypto/tls"
	"crypto/x509"
	"flag"
	"fmt"
	"html/template"
	"io/ioutil"
	"log"
	"net/http"
	"os"
	"os/signal"
	"time"

	"github.com/gorilla/mux"
	"github.com/yosssi/gmq/mqtt"
	"github.com/yosssi/gmq/mqtt/client"
)

var cli *client.Client

func main() {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, os.Kill)

	caFile := flag.String("ca", "", "path to certificate authority file")
	flag.Parse()

	// Load CA cert
	caCert, err := ioutil.ReadFile(*caFile)
	if err != nil {
		log.Fatal(err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	// Create an MQTT Client.
	cli = client.New(&client.Options{
		// Define the processing of the error handler.
		ErrorHandler: func(err error) {
			log.Printf("error handler: %v", err)
		},
	})

	// Connect to the MQTT Server.
	err = cli.Connect(&client.ConnectOptions{
		Network: "tcp",
		Address: "192.168.178.2:1883",
		TLSConfig: &tls.Config{
			RootCAs:            caCertPool,
			InsecureSkipVerify: true, // FIXME
		},
		ClientID: []byte("hau"),
		UserName: []byte("hau"),
		Password: []byte("xug7deiv1aiph3EiTha7"),
	})
	if err != nil {
		log.Fatalf("connect failed: %v", err)
	}

	cli.Subscribe(&client.SubscribeOptions{SubReqs: []*client.SubReq{{
		TopicFilter: []byte("+/socket01/+"),
		Handler: func(topicName, message []byte) {
			log.Printf("%s: %s", string(topicName), string(message))
		},
	}}})

	r := mux.NewRouter()
	r.HandleFunc("/", homeHandler).Methods("GET")
	r.PathPrefix("/static/").Handler(http.StripPrefix("/static/", http.FileServer(http.Dir("static"))))
	r.HandleFunc("/api/{device}", socketHandler).Methods("PUT")

	srv := &http.Server{
		Handler: r,
		Addr:    ":8080",
		// Good practice: enforce timeouts for servers you create!
		WriteTimeout: 15 * time.Second,
		ReadTimeout:  15 * time.Second,
	}

	go func() {
		if err := srv.ListenAndServe(); err != nil {
			log.Printf("listen and serve: %v", err)
		}
	}()

	// Wait for receiving a signal.
	<-sigc

	// Disconnect the Network Connection.
	if err := cli.Disconnect(); err != nil {
		log.Fatalf("disconnect failed: %v", err)
	}
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
	err = tmpl.Execute(w, &home{
		Lamps: []lamp{
			{"Stehlampe", "socket01"},
		},
	})

	if err != nil {
		log.Printf("unable to execute template: %v", err)
	}
}

type lamp struct {
	Name   string
	Device string
}

type home struct {
	Lamps []lamp
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
	if err = sendCommand(device, status); err != nil {
		log.Printf("send command failed: %v", err)
		w.WriteHeader(500)
		return
	}

	w.WriteHeader(http.StatusOK)
}

func sendCommand(device string, status bool) error {
	statusStr := ""
	if status {
		statusStr = "1"
	} else {
		statusStr = "0"
	}

	return cli.Publish(&client.PublishOptions{
		QoS:       mqtt.QoS2,
		TopicName: []byte(fmt.Sprintf("cmnd/%s/POWER", device)),
		Message:   []byte(statusStr),
	})
}
