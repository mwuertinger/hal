package device

import (
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mwuertinger/hal/pkg/config"
	"github.com/mwuertinger/hal/pkg/mqtt"
)

// reset clears the package registry between tests. The registry is package
// state because main wires it once at startup; tests need it back.
func reset(t *testing.T, broker mqtt.Broker) {
	t.Helper()

	mu.Lock()
	devices = make(map[string]Device)
	mu.Unlock()

	done = make(chan struct{})
	shutdownOnce = sync.Once{}

	SetMqttBroker(broker)
	t.Cleanup(func() {
		mu.Lock()
		devs := make([]Device, 0, len(devices))
		for _, d := range devices {
			devs = append(devs, d)
		}
		devices = make(map[string]Device)
		mu.Unlock()
		for _, d := range devs {
			d.Shutdown()
		}
	})
}

func registerOne(t *testing.T, broker mqtt.Broker) Switch {
	t.Helper()

	reset(t, broker)
	err := RegisterDevices([]config.Device{
		{ID: "lamp1", Name: "Floor Lamp", Location: "Living Room", Type: config.DeviceTypeSonoffMqttSwitch},
	})
	if err != nil {
		t.Fatal(err)
	}
	return Get("lamp1").(Switch)
}

// TestStateIsQueriedAtStartup: without the query, every lamp reads "off" from
// startup until its next status message - up to five minutes with Tasmota's
// default TelePeriod, and forever for one that is unplugged.
func TestStateIsQueriedAtStartup(t *testing.T) {
	broker := mqtt.NewFake()
	registerOne(t, broker)

	var queried bool
	for _, p := range broker.Published() {
		if p.Topic == "cmnd/lamp1/POWER" && p.Msg == "" {
			queried = true
		}
	}
	if !queried {
		t.Errorf("no state query was published: %v", broker.Published())
	}
}

// TestSubscribeFailureIsFatal: a device whose subscription failed is in the UI,
// permanently reporting off, with one line in the startup log to say why.
func TestSubscribeFailureIsFatal(t *testing.T) {
	broker := mqtt.NewFake()
	broker.SubscribeErr = errors.New("not authorised")
	reset(t, broker)

	err := RegisterDevices([]config.Device{
		{ID: "lamp1", Name: "Floor Lamp", Location: "Living Room", Type: config.DeviceTypeSonoffMqttSwitch},
	})
	if err == nil {
		t.Fatal("RegisterDevices() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "subscribe") {
		t.Errorf("RegisterDevices() = %q, want it to name the failed subscription", err)
	}
}

func TestNotificationsUpdateState(t *testing.T) {
	broker := mqtt.NewFake()
	dev := registerOne(t, broker)

	events := dev.Events()

	for _, tc := range []struct {
		topic, msg string
		want       bool
	}{
		{"stat/lamp1/POWER", "ON", true},
		{"stat/lamp1/POWER", "OFF", false},
		{"tele/lamp1/STATE", `{"Time":"2026-01-01T00:00:00","POWER":"ON"}`, true},
	} {
		broker.Deliver(tc.topic, tc.msg)

		select {
		case event := <-events:
			payload, ok := event.Payload.(EventPayloadSwitch)
			if !ok {
				t.Fatalf("payload = %T, want EventPayloadSwitch", event.Payload)
			}
			if payload.State != tc.want {
				t.Errorf("%s %q: state = %v, want %v", tc.topic, tc.msg, payload.State, tc.want)
			}
		case <-time.After(2 * time.Second):
			t.Fatalf("%s %q: no event", tc.topic, tc.msg)
		}

		if got := dev.LastKnownState(); got != tc.want {
			t.Errorf("%s %q: LastKnownState() = %v, want %v", tc.topic, tc.msg, got, tc.want)
		}
	}
}

// TestMalformedNotificationsAreIgnored: the zero value of the parsed state is
// "off", so anything that falls through without an error records the lamp as
// off and tells every browser so.
func TestMalformedNotificationsAreIgnored(t *testing.T) {
	broker := mqtt.NewFake()
	dev := registerOne(t, broker)

	events := dev.Events()
	broker.Deliver("stat/lamp1/POWER", "ON")
	<-events

	for _, msg := range []string{"MAYBE", "", "1"} {
		broker.Deliver("stat/lamp1/POWER", msg)
	}
	broker.Deliver("tele/lamp1/STATE", `not json`)
	broker.Deliver("tele/lamp1/STATE", `{"POWER":"MAYBE"}`)

	select {
	case event := <-events:
		t.Errorf("a malformed notification produced an event: %+v", event)
	case <-time.After(200 * time.Millisecond):
	}

	if !dev.LastKnownState() {
		t.Error("LastKnownState() = false, want the last valid state to survive")
	}
}

// TestSlowObserverDoesNotBlockTheDevice is the regression test for the SIGTERM
// deadlock: the device mutex used to be held across an unbuffered send to every
// observer, so one consumer that stopped reading blocked LastKnownState() -
// which every page load needs - and left the handler unable to see its own
// shutdown channel.
func TestSlowObserverDoesNotBlockTheDevice(t *testing.T) {
	broker := mqtt.NewFake()
	dev := registerOne(t, broker)

	dev.Events() // never read from

	for i := 0; i < observerQueue*3; i++ {
		state := "ON"
		if i%2 == 0 {
			state = "OFF"
		}
		broker.Deliver("stat/lamp1/POWER", state)
	}

	done := make(chan bool, 1)
	go func() { done <- dev.LastKnownState() }()

	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("LastKnownState() blocked behind an observer that stopped reading")
	}

	shut := make(chan struct{})
	go func() { dev.Shutdown(); close(shut) }()

	select {
	case <-shut:
	case <-time.After(2 * time.Second):
		t.Fatal("Shutdown() blocked behind an observer that stopped reading")
	}
}

// TestEventsRegistersBeforeReturning: d.Events() is what adds the observer, so
// deferring it to the forwarder goroutine left a window in which events were
// dropped before anybody was listening.
func TestEventsRegistersBeforeReturning(t *testing.T) {
	broker := mqtt.NewFake()
	registerOne(t, broker)

	events := Events()
	broker.Deliver("stat/lamp1/POWER", "ON")

	select {
	case event := <-events:
		if event.DeviceId != "lamp1" {
			t.Errorf("DeviceId = %q, want lamp1", event.DeviceId)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("the first event after Events() was dropped")
	}
}

func TestRegisterDevicesRejections(t *testing.T) {
	for _, tc := range []struct {
		name    string
		devices []config.Device
		wantErr string
	}{
		{
			name:    "duplicate id",
			devices: []config.Device{{ID: "a", Name: "A", Type: config.DeviceTypeSonoffMqttSwitch}, {ID: "a", Name: "B", Type: config.DeviceTypeSonoffMqttSwitch}},
			wantErr: "duplicate device id",
		},
		{
			name:    "unknown type",
			devices: []config.Device{{ID: "a", Name: "A", Type: "teapot"}},
			wantErr: "invalid typ",
		},
		{
			name:    "empty id",
			devices: []config.Device{{ID: "", Name: "A", Type: config.DeviceTypeSonoffMqttSwitch}},
			wantErr: "invalid id",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			reset(t, mqtt.NewFake())
			err := RegisterDevices(tc.devices)
			if err == nil {
				t.Fatalf("RegisterDevices() = nil, want an error containing %q", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("RegisterDevices() = %q, want an error containing %q", err, tc.wantErr)
			}
		})
	}
}

func TestSwitchPublishesCommand(t *testing.T) {
	broker := mqtt.NewFake()
	dev := registerOne(t, broker)

	if err := dev.Switch(true); err != nil {
		t.Fatal(err)
	}
	if err := dev.Switch(false); err != nil {
		t.Fatal(err)
	}

	var commands []string
	for _, p := range broker.Published() {
		if p.Topic == "cmnd/lamp1/POWER" && p.Msg != "" {
			commands = append(commands, p.Msg)
		}
	}
	if len(commands) != 2 || commands[0] != "1" || commands[1] != "0" {
		t.Errorf("published %v, want [1 0]", commands)
	}
}

// TestUnexpectedTopicIsRejected: the parsed state's zero value is "off", so a
// topic that matches neither case must be an error rather than falling through
// to record the lamp as off and broadcast it. Unreachable through the broker
// while both subscriptions are exact topics, hence the direct call - it is one
// wildcard subscription away from being reachable.
func TestUnexpectedTopicIsRejected(t *testing.T) {
	broker := mqtt.NewFake()
	dev := registerOne(t, broker).(*sonoffMqttSwitch)

	broker.Deliver("stat/lamp1/POWER", "ON")
	deadline := time.Now().Add(2 * time.Second)
	for !dev.LastKnownState() && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}

	err := dev.processNotification(mqtt.Notification{Topic: "tele/lamp1/SENSOR", Msg: `{"POWER":"OFF"}`})
	if err == nil {
		t.Fatal("processNotification() of an unsubscribed topic = nil, want an error")
	}
	if !strings.Contains(err.Error(), "unexpected topic") {
		t.Errorf("processNotification() = %q, want it to name the topic", err)
	}
	if !dev.LastKnownState() {
		t.Error("LastKnownState() = false: an unexpected topic overwrote the real state")
	}
}

// TestStateIsQueriedAgainAfterAReconnect: nothing is retained while the
// connection is down and the session is clean, so every transition during an
// outage is lost - a lamp switched by its own button, or by another client.
// Without asking again, the UI shows the pre-outage state until the device's
// next telemetry frame, up to five minutes later.
func TestStateIsQueriedAgainAfterAReconnect(t *testing.T) {
	broker := mqtt.NewFake()
	registerOne(t, broker)

	before := len(broker.Published())
	broker.Reconnect()

	var queries int
	for _, p := range broker.Published()[before:] {
		if p.Topic == "cmnd/lamp1/POWER" && p.Msg == "" {
			queries++
		}
	}
	if queries != 1 {
		t.Errorf("%d state queries after a reconnect, want 1: %v", queries, broker.Published()[before:])
	}
}

// TestBothTopicsShareOneChannel: the device's two topics contradict each other
// by design, so they have to arrive in the order the broker sent them. Two
// subscriptions read by a select would leave that order to the scheduler - and
// a tele/<id>/STATE frame restating the old state, applied after the
// stat/<id>/POWER that superseded it, records the lamp in the state it just
// left.
func TestBothTopicsShareOneChannel(t *testing.T) {
	broker := mqtt.NewFake()
	dev := registerOne(t, broker)

	if got := len(broker.Channels()); got != 1 {
		t.Fatalf("the device holds %d subscription channels, want 1", got)
	}

	events := dev.Events()

	// The pair that inverts: telemetry restating "off", immediately followed by
	// the echo of a command that turned it on.
	broker.Deliver("tele/lamp1/STATE", `{"POWER":"OFF"}`)
	broker.Deliver("stat/lamp1/POWER", "ON")

	var last bool
	for i := 0; i < 2; i++ {
		select {
		case event := <-events:
			last = event.Payload.(EventPayloadSwitch).State
		case <-time.After(2 * time.Second):
			t.Fatalf("only %d of 2 events arrived", i)
		}
	}

	if !last {
		t.Error("the last event was off: the two topics were applied out of order")
	}
	if !dev.LastKnownState() {
		t.Error("LastKnownState() = false, want the later message to win")
	}
}
