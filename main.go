package main

import (
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"

	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/device"
	"github.com/mwuertinger/hal/pkg/frontend"
	"github.com/mwuertinger/hal/pkg/mqtt"
)

// exitConfig is sysexits.h's EX_CONFIG. hal.service names it in
// RestartPreventExitStatus=, so a config HAL can never load stops the unit
// instead of restarting it every five seconds until someone reads the journal.
const exitConfig = 78

func main() {
	// journald stamps every entry itself, and this is a systemd service, so
	// the log package's own timestamp is only a second copy of it.
	log.SetFlags(0)

	sigc := make(chan os.Signal, 2)
	// Not os.Kill: SIGKILL cannot be caught, so listing it only suggests it can.
	signal.Notify(sigc, os.Interrupt, syscall.SIGTERM)

	configPath := flag.String("config", "", "Path to config file.")
	flag.Parse()

	if len(*configPath) < 1 {
		log.Print("Missing -config argument.")
		os.Exit(exitConfig)
	}

	c, err := config.Load(*configPath)
	if err != nil {
		log.Printf("Failed to read config file: %v", err)
		os.Exit(exitConfig)
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

	log.Println("Server ready")

	// Wait for receiving a signal.
	sig := <-sigc
	log.Printf("Received %v signal, shutting down...", sig)

	// A second signal exits immediately: a shutdown that cannot finish should
	// not need SIGKILL to escape.
	go func() {
		sig := <-sigc
		log.Printf("Received %v signal again, exiting now", sig)
		os.Exit(1)
	}()

	// Outside in: stop serving, then stop the devices producing events, then
	// close the broker connection they publish through.
	frontend.Shutdown()
	device.Shutdown()
	mqttBroker.Shutdown()
}
