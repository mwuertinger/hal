package main

import (
	"flag"
	"log"
	"os"
	"os/signal"

	"github.com/mwuertinger/hau/pkg/config"
	"github.com/mwuertinger/hau/pkg/frontend"
	"github.com/mwuertinger/hau/pkg/mqtt"
	"github.com/yosssi/gmq/mqtt/client"
)

var cli *client.Client

func main() {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, os.Kill)

	configPath := flag.String("config", "", "Path to config file.")
	flag.Parse()

	if len(*configPath) < 1 {
		log.Fatalf("Missing -config PATH argument.")
	}

	c, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to read config file: %v", err)
	}

	mqttService := mqtt.New()
	if err := mqttService.Connect(c.Mqtt); err != nil {
		log.Fatalf("mqttService.Connect: %v", err)
	}

	if err := frontend.Start(c.Http, mqttService); err != nil {
		log.Fatalf("frontend.Start: %v", err)
	}

	// Wait for receiving a signal.
	<-sigc

	if err := frontend.Shutdown(); err != nil {
		log.Printf("frontend.Shutdown: %v", err)
	}

	// Disconnect the Network Connection.
	if err := mqttService.Disconnect(); err != nil {
		log.Printf("mqttService.Disconnect: %v", err)
	}
}
