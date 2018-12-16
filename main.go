package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/device"
	"github.com/mwuertinger/hal/pkg/frontend"
	"github.com/mwuertinger/hal/pkg/mqtt"
	"github.com/mwuertinger/hal/pkg/persistence"
	"github.com/mwuertinger/hal/pkg/timer"
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

	persistence := persistence.GetInMemoryService()
	if err := persistence.Start(); err != nil {
		log.Fatalf("persistence.Start: %v", err)
	}

	timerSvc := timer.NewService()
	if err := timerSvc.Start(); err != nil {
		log.Fatalf("timerSvc.Start: %v", err)
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

	// TODO remove
	var switches []device.Switch
	for _, dev := range device.List() {
		switches = append(switches, dev.(device.Switch))
	}
	timerSvc.AddJob(timer.Job{
		Timestamp: time.Date(2018, 10, 26, 5, 0, 0, 0, time.UTC),
		Status:    true,
		Switches:  switches,
	})

	log.Println("Server ready")

	// Wait for receiving a signal.
	sig := <-sigc
	log.Printf("Received %v signal, shutting down...", sig)

	frontend.Shutdown()
	mqttBroker.Shutdown()
	device.Shutdown()
}
