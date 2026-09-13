package device

import (
	"encoding/json"
	"fmt"
	"log"
	"sync"

	"github.com/mwuertinger/hal/pkg/mqtt"
)

// observerQueue is how far one event consumer may fall behind before its
// events are dropped. Sending must never block: the sender is the goroutine
// that also owns this device's state, so a consumer that stops reading would
// otherwise stop the device from tracking its lamp at all.
const observerQueue = 64

type sonoffMqttSwitch struct {
	device
	shutdownChan chan interface{}
	shutdownOnce sync.Once
	wg           sync.WaitGroup

	mu             sync.Mutex
	lastKnownState bool
	observers      []chan Event
}

func NewSonoffMqttSwitch(id string, name string, location string) (Switch, error) {
	dev := &sonoffMqttSwitch{
		device: device{
			id:       id,
			name:     name,
			location: location,
		},
		shutdownChan: make(chan interface{}),
	}

	// A failed subscription used to be logged and then ignored, which left the
	// device in the UI, permanently reporting off, with one startup log line to
	// explain why. It is a configuration or broker-ACL problem, so fail to start.
	powerChan, err := broker().Subscribe(dev.powerTopic())
	if err != nil {
		return nil, fmt.Errorf("subscribe to %s: %w", dev.powerTopic(), err)
	}
	stateChan, err := broker().Subscribe(dev.stateTopic())
	if err != nil {
		return nil, fmt.Errorf("subscribe to %s: %w", dev.stateTopic(), err)
	}

	dev.wg.Add(1)
	go dev.notificationHandler(powerChan, stateChan)

	// Ask the device where it stands. Without this every lamp reads "off" from
	// startup until its next status message - up to TelePeriod, five minutes by
	// default, and forever for one that is unplugged - and a tap on a lamp that
	// is really on would then send it the state it is already in. An empty
	// payload on the command topic is a Tasmota status query: it reports on
	// stat/<id>/POWER and does not switch anything.
	if err := broker().Publish(dev.commandTopic(), ""); err != nil {
		// Not fatal: the device answers the next tele/<id>/STATE anyway, and
		// refusing to start over a lamp that is merely unplugged would be worse.
		log.Printf("%v: state query failed: %v", dev.id, err)
	}

	return dev, nil
}

func (s *sonoffMqttSwitch) powerTopic() string   { return fmt.Sprintf("stat/%s/POWER", s.id) }
func (s *sonoffMqttSwitch) stateTopic() string   { return fmt.Sprintf("tele/%s/STATE", s.id) }
func (s *sonoffMqttSwitch) commandTopic() string { return fmt.Sprintf("cmnd/%s/POWER", s.id) }

func (s *sonoffMqttSwitch) notificationHandler(powerChan, stateChan <-chan mqtt.Notification) {
	defer func() {
		s.mu.Lock()
		for _, observer := range s.observers {
			close(observer)
		}
		s.observers = nil
		s.mu.Unlock()

		log.Printf("%v: shutdown complete", s.id)
		s.wg.Done()
	}()

	for {
		select {
		case notification, ok := <-powerChan:
			if !ok {
				return
			}
			if err := s.processNotification(notification); err != nil {
				log.Printf("processNotification: %v", err)
			}

		case notification, ok := <-stateChan:
			if !ok {
				return
			}
			if err := s.processNotification(notification); err != nil {
				log.Printf("processNotification: %v", err)
			}

		case <-s.shutdownChan:
			return
		}
	}
}

func (s *sonoffMqttSwitch) processNotification(notification mqtt.Notification) error {
	var state bool
	var err error

	switch notification.Topic {
	case s.stateTopic():
		// example: {"Time":"2018-04-29T09:03:46","Uptime":"5T12:40:16","Vcc":3.405,"POWER":"OFF","Wifi":{"AP":1,"SSId":"Miichsoft","RSSI":100,"APMac":"34:81:C4:07:12:78"}}}
		var obj struct {
			Power string `json:"POWER"`
		}
		err = json.Unmarshal([]byte(notification.Msg), &obj)
		if err != nil {
			return fmt.Errorf("unmarshal json: %v, json: %s", err, notification.Msg)
		}

		state, err = toState(obj.Power)
		if err != nil {
			return err
		}

	case s.powerTopic():
		state, err = toState(notification.Msg)
		if err != nil {
			return err
		}

	default:
		// Unreachable while both subscriptions are exact topics, but the zero
		// value of state is "off", so falling through here would record the
		// lamp as off and tell every browser so.
		return fmt.Errorf("unexpected topic: %s", notification.Topic)
	}

	event := Event{
		Timestamp: notification.Timestamp,
		DeviceId:  s.id,
		Payload:   EventPayloadSwitch{state},
	}

	s.mu.Lock()
	s.lastKnownState = state
	observers := s.observers
	s.mu.Unlock()

	// Deliberately outside the lock, and non-blocking. Holding s.mu across a
	// send let one stalled consumer wedge LastKnownState() - which every page
	// load needs - and blocked this goroutine where it could no longer see
	// shutdownChan, so SIGTERM hung until systemd killed the process.
	for _, observer := range observers {
		select {
		case observer <- event:
		default:
			log.Printf("%v: observer %d events behind, dropping", s.id, observerQueue)
		}
	}

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

	return broker().Publish(s.commandTopic(), stateStr)
}

func (s *sonoffMqttSwitch) Events() <-chan Event {
	observer := make(chan Event, observerQueue)
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
	switch str {
	case "ON":
		return true, nil
	case "OFF":
		return false, nil
	default:
		return false, fmt.Errorf("invalid switch state: %s", str)
	}
}

// Shutdown is idempotent: closing shutdownChan twice is a panic, and a caller
// that shuts a device down individually should not be a trap for the registry
// doing the same afterwards.
func (s *sonoffMqttSwitch) Shutdown() {
	s.shutdownOnce.Do(func() { close(s.shutdownChan) })
	s.wg.Wait()
}
