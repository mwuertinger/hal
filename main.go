package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mwuertinger/hau/pkg/config"
	"github.com/mwuertinger/hau/pkg/device"
	"github.com/mwuertinger/hau/pkg/frontend"
	"github.com/mwuertinger/hau/pkg/mqtt"
)

func main() {
	sigc := make(chan os.Signal, 1)
	signal.Notify(sigc, os.Interrupt, os.Kill, syscall.SIGTERM)

	configPath := flag.String("config", "", "Path to config file.")
	flag.Parse()

	if len(*configPath) < 1 {
		log.Fatalf("Missing -config argument.")
	}

	c, err := config.Load(*configPath)
	if err != nil {
		log.Fatalf("Failed to read config file: %v", err)
	}

	mqttBroker := mqtt.New()
	if err := mqttBroker.Connect(c.Mqtt); err != nil {
		log.Fatalf("mqttBroker.Connect: %v", err)
	}

	device.SetMqttBroker(mqttBroker)
	if err := device.RegisterDevices(c.Devices); err != nil {
		log.Fatalf("device.RegisterDevices: %v", err)
	}

	if err := frontend.Start(c.Http); err != nil {
		log.Fatalf("frontend.Start: %v", err)
	}

	// Wait for receiving a signal.
	<-sigc

	frontend.Shutdown()
	mqttBroker.Shutdown()
	device.Shutdown()
}
