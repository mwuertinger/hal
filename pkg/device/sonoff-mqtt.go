package device

import (
	"fmt"
)

type sonoffMqttSwitch struct {
	device
}

func (s *sonoffMqttSwitch) ID() string {
	return s.id
}

func (s *sonoffMqttSwitch) Name() string {
	return s.name
}

func (s *sonoffMqttSwitch) Location() string {
	return s.location
}

func (s *sonoffMqttSwitch) Switch(status bool) error {
	statusStr := ""
	if status {
		statusStr = "1"
	} else {
		statusStr = "0"
	}

	return mqttBroker.Publish(fmt.Sprintf("cmnd/%s/POWER", s.id), statusStr)
}
