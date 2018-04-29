package device

import (
	"fmt"
	"github.com/mwuertinger/hau/pkg/config"
	"github.com/mwuertinger/hau/pkg/mqtt"
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
	AddObserver(chan<- Event)
}

type Switch interface {
	Device
	Switch(status bool) error
	LastKnownState() bool
}

var (
	mqttBroker mqtt.Broker
	devices    map[string]Device
)

func init() {
	devices = make(map[string]Device)
}

func SetMqttBroker(broker mqtt.Broker) {
	mqttBroker = broker
}

func RegisterDevices(deviceConfig []config.Device) error {
	for _, c := range deviceConfig {
		if err := addDevice(c.ID, c.Name, c.Location, c.Type); err != nil {
			return err
		}
	}
	return nil
}

func addDevice(id, name, location, typ string) error {
	if len(id) < 1 {
		return fmt.Errorf("invalid id: %s", id)
	}
	if devices[id] != nil {
		return fmt.Errorf("duplicate device id: %s", id)
	}
	if len(name) < 1 {
		return fmt.Errorf("invalid name: %s", name)
	}

	var dev Device

	switch typ {
	case "sonoff-mqtt-switch":
		dev = NewSonoffMqttSwitch(id, name, location)
	default:
		return fmt.Errorf("invalid typ: %s", typ)
	}

	devices[id] = dev

	return nil
}

func List() []Device {
	list := make([]Device, 0, len(devices))
	for _, d := range devices {
		list = append(list, d)
	}
	return list
}

func Get(id string) Device {
	return devices[id]
}

func AddObserver(observer chan<- Event) {
	for _, dev := range devices {
		dev.AddObserver(observer)
	}
}
