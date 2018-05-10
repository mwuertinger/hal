package device

import (
	"encoding/json"
	"fmt"
	"github.com/mwuertinger/hal/pkg/mqtt"
	"log"
	"sync"
)

type sonoffMqttSwitch struct {
	device
	lastKnownState bool
	observers      []chan<- Event
	shutdownChan   chan interface{}
	wg             sync.WaitGroup
	mu             sync.Mutex
}

func NewSonoffMqttSwitch(id string, name string, location string) Switch {
	dev := &sonoffMqttSwitch{
		device: device{
			id:       id,
			name:     name,
			location: location,
		},
		shutdownChan: make(chan interface{}),
	}

	powerChan, err := mqttBroker.Subscribe(fmt.Sprintf("stat/%s/POWER", id))
	if err != nil {
		log.Printf("sonoffMqttSwitch.Observe: %v", err)
	}
	stateChan, err := mqttBroker.Subscribe(fmt.Sprintf("tele/%s/STATE", id))
	if err != nil {
		log.Printf("sonoffMqttSwitch.Observe: %v", err)
	}

	dev.wg.Add(1)

	go dev.notificationHandler(powerChan, stateChan)

	return dev
}

func (s *sonoffMqttSwitch) notificationHandler(powerChan, stateChan <-chan mqtt.Notification) {
	for {
		select {
		case notification, ok := <-powerChan:
			if !ok {
				goto shutdown
			}
			if err := s.processNotification(notification); err != nil {
				log.Printf("processNotification: %v", err)
			}

		case notification, ok := <-stateChan:
			if !ok {
				goto shutdown
			}
			if err := s.processNotification(notification); err != nil {
				log.Printf("processNotification: %v", err)
			}
		case <-s.shutdownChan:
			goto shutdown
		}
	}

shutdown:
	s.mu.Lock()
	for _, observer := range s.observers {
		close(observer)
	}
	s.observers = nil
	s.mu.Unlock()

	log.Printf("%v: shutdown complete", s.id)
	s.wg.Done()

}

func (s *sonoffMqttSwitch) processNotification(notification mqtt.Notification) error {
	var state bool
	var err error

	if notification.Topic == fmt.Sprintf("tele/%s/STATE", s.id) {
		// example: {"Time":"2018-04-29T09:03:46","Uptime":"5T12:40:16","Vcc":3.405,"POWER":"OFF","Wifi":{"AP":1,"SSId":"Miichsoft","RSSI":100,"APMac":"34:81:C4:07:12:78"}}}
		var obj struct {
			Power string `json:"POWER"`
		}
		err = json.Unmarshal([]byte(notification.Msg), &obj)
		if err != nil {
			return fmt.Errorf("unmarshal json: %v, json: %s", err, notification.Msg)
		}

		if obj.Power == "ON" {
			state = true
		} else if obj.Power == "OFF" {
			state = false
		} else {
			return fmt.Errorf("invalid POWER: %s", obj.Power)
		}

	} else if notification.Topic == fmt.Sprintf("stat/%s/POWER", s.id) {
		state, err = toState(notification.Msg)
		if err != nil {
			return err
		}
	}

	event := Event{
		Timestamp: notification.Timestamp,
		DeviceId:  s.id,
		Payload:   EventPayloadSwitch{state},
	}

	s.mu.Lock()
	s.lastKnownState = state
	for _, observer := range s.observers {
		observer <- event
	}
	s.mu.Unlock()

	return nil
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

func (s *sonoffMqttSwitch) Events() <-chan Event {
	observer := make(chan Event)
	s.mu.Lock()
	s.observers = append(s.observers, observer)
	s.mu.Unlock()
	return observer
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

func (s *sonoffMqttSwitch) Shutdown() {
	close(s.shutdownChan)
	s.wg.Wait()
}
