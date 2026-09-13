package device

import (
	"fmt"
	"log"
	"sort"
	"strings"
	"sync"

	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/mqtt"
)

type device struct {
	id       string
	name     string
	location string
}

type Device interface {
	ID() string
	Name() string
	Location() string
	Events() <-chan Event
	Shutdown()
}

type Switch interface {
	Device
	Switch(status bool) error
	LastKnownState() bool
}

var (
	// mu guards everything below it. The registry is written once at startup
	// and read by every HTTP request afterwards, so the lock is uncontended -
	// but it is what keeps that true if anything ever registers a device late.
	mu         sync.RWMutex
	mqttBroker mqtt.Broker
	devices    = make(map[string]Device)

	// done is closed by Shutdown to release the fan-in goroutines in Events().
	done         = make(chan struct{})
	shutdownOnce sync.Once
)

// broker returns the configured MQTT broker.
func broker() mqtt.Broker {
	mu.RLock()
	defer mu.RUnlock()
	return mqttBroker
}

func SetMqttBroker(b mqtt.Broker) {
	mu.Lock()
	defer mu.Unlock()
	mqttBroker = b
}

func RegisterDevices(deviceConfig []config.Device) error {
	if broker() == nil {
		return fmt.Errorf("no MQTT broker set")
	}

	for _, c := range deviceConfig {
		if err := addDevice(c.ID, c.Name, c.Location, c.Type); err != nil {
			return err
		}
	}

	log.Printf("Registered %d device(s)", len(deviceConfig))

	return nil
}

func addDevice(id, name, location string, typ config.DeviceType) error {
	if len(id) < 1 {
		return fmt.Errorf("invalid id: %s", id)
	}
	if len(name) < 1 {
		return fmt.Errorf("invalid name: %s", name)
	}

	mu.Lock()
	_, duplicate := devices[id]
	mu.Unlock()
	if duplicate {
		return fmt.Errorf("duplicate device id: %s", id)
	}

	var dev Device

	switch typ {
	case config.DeviceTypeSonoffMqttSwitch:
		// Constructed outside the lock: it subscribes and publishes, and
		// holding the registry lock across broker I/O would block every
		// in-flight page load for the duration.
		sw, err := NewSonoffMqttSwitch(id, name, location)
		if err != nil {
			return fmt.Errorf("device %s: %w", id, err)
		}
		dev = sw
	default:
		return fmt.Errorf("invalid typ: %s", typ)
	}

	mu.Lock()
	defer mu.Unlock()
	devices[id] = dev

	return nil
}

func List() []Device {
	mu.RLock()
	defer mu.RUnlock()

	list := make([]Device, 0, len(devices))
	for _, d := range devices {
		list = append(list, d)
	}
	sort.Slice(list, func(i, j int) bool {
		return strings.Compare(list[i].ID(), list[j].ID()) < 0
	})
	return list
}

func Get(id string) Device {
	mu.RLock()
	defer mu.RUnlock()
	return devices[id]
}

// Events merges the event streams of every registered device into one channel,
// which is closed once every device has shut down.
func Events() <-chan Event {
	out := make(chan Event)

	devs := List()

	var wg sync.WaitGroup
	wg.Add(len(devs))

	for _, dev := range devs {
		// Registered here rather than inside the goroutine: d.Events() is what
		// adds the observer, so deferring it to a goroutine left a window in
		// which the device had already published events nobody was listening for.
		events := dev.Events()

		go func() {
			defer wg.Done()
			for event := range events {
				select {
				case out <- event:
				case <-done:
					return
				}
			}
		}()
	}

	go func() {
		wg.Wait()
		close(out)
	}()

	return out
}

func Shutdown() {
	shutdownOnce.Do(func() { close(done) })

	mu.RLock()
	devs := make([]Device, 0, len(devices))
	for _, d := range devices {
		devs = append(devs, d)
	}
	mu.RUnlock()

	for _, dev := range devs {
		dev.Shutdown()
	}
	log.Println("Devices shutdown complete")
}
