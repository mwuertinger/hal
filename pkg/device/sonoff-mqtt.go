package device

import (
	"fmt"
	"github.com/mwuertinger/hau/pkg/mqtt"
	"log"
	"sync"
)

type sonoffMqttSwitch struct {
	device
	lastKnownState bool
	observers      []chan<- Event
	mu             sync.Mutex
}

func NewSonoffMqttSwitch(id string, name string, location string) Switch {
	dev := &sonoffMqttSwitch{
		device: device{
			id:       id,
			name:     name,
			location: location,
		},
	}

	notificationChan := make(chan mqtt.Notification)
	if err := mqttBroker.Subscribe(fmt.Sprintf("stat/%s/POWER", id), notificationChan); err != nil {
		log.Println("sonoffMqttSwitch.Observe: %v", err)
	}

	go func() {
		for notification := range notificationChan {
			state, err := toState(notification.Msg)
			if err != nil {
				log.Print(err)
				continue
			}

			event := Event{
				Timestamp: notification.Timestamp,
				DeviceId:  dev.id,
				Payload:   EventPayloadSwitch{state},
			}

			dev.mu.Lock()
			dev.lastKnownState = state
			for _, observer := range dev.observers {
				observer <- event
			}
			dev.mu.Unlock()
		}

		log.Println("sonoffMqttSwitch: observer shutting down")

		dev.mu.Lock()
		for _, observer := range dev.observers {
			close(observer)
		}
		dev.mu.Unlock()
	}()

	return dev
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

func (s *sonoffMqttSwitch) Switch(state bool) error {
	stateStr := "0"
	if state {
		stateStr = "1"
	}

	return mqttBroker.Publish(fmt.Sprintf("cmnd/%s/POWER", s.id), stateStr)
}

func (s *sonoffMqttSwitch) AddObserver(observer chan<- Event) {
	s.mu.Lock()
	s.observers = append(s.observers, observer)
	s.mu.Unlock()
}

func (s *sonoffMqttSwitch) LastKnownState() bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.lastKnownState
}

func toState(str string) (bool, error) {
	if str == "OFF" {
		return false, nil
	} else if str == "ON" {
		return true, nil
	} else {
		return false, fmt.Errorf("invalid switch state: %s", str)
	}
}
