package mqtt

import (
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

// TestDeliverDuringShutdown is the regression test for the shutdown panic: the
// broker used to close the subscriber channels while a message handler was
// parked on a send into one, which is "send on closed channel" - unrecoverable,
// and enough to mark a clean systemctl stop as failed.
func TestDeliverDuringShutdown(t *testing.T) {
	for i := 0; i < 50; i++ {
		b := &broker{subs: make(map[string][]chan Notification)}
		c := make(chan Notification, 1)
		b.subs["stat/lamp/POWER"] = []chan Notification{c}

		var wg sync.WaitGroup
		wg.Add(2)
		go func() {
			defer wg.Done()
			for j := 0; j < 20; j++ {
				b.deliver("stat/lamp/POWER", "ON")
			}
		}()
		go func() {
			defer wg.Done()
			b.Shutdown()
		}()
		wg.Wait()
	}
}

// TestDeliverNeverBlocks: paho delivers from the single goroutine that keeps
// messages in order, so waiting on one slow consumer would hold up every other
// subscriber - and back up into the connection.
func TestDeliverNeverBlocks(t *testing.T) {
	b := &broker{subs: make(map[string][]chan Notification)}
	c := make(chan Notification, 2)
	b.subs["t"] = []chan Notification{c}

	done := make(chan struct{})
	go func() {
		for i := 0; i < 100; i++ {
			b.deliver("t", "ON")
		}
		close(done)
	}()

	<-done
	if len(c) != 2 {
		t.Errorf("queue holds %d, want it capped at its capacity of 2", len(c))
	}
}

func TestSubscribeFansOutToEveryChannel(t *testing.T) {
	b := &broker{subs: make(map[string][]chan Notification)}
	first := make(chan Notification, 1)
	second := make(chan Notification, 1)
	b.subs["t"] = []chan Notification{first, second}

	b.deliver("t", "ON")

	for name, c := range map[string]chan Notification{"first": first, "second": second} {
		select {
		case n := <-c:
			if n.Msg != "ON" {
				t.Errorf("%s: Msg = %q, want ON", name, n.Msg)
			}
		default:
			t.Errorf("%s subscriber got nothing", name)
		}
	}
}

func TestUnsubscribeRemovesOnlyItsOwnChannel(t *testing.T) {
	b := &broker{subs: make(map[string][]chan Notification)}
	keep := make(chan Notification, 1)
	drop := make(chan Notification, 1)
	b.subs["t"] = []chan Notification{keep, drop}

	b.unsubscribe("t", drop)

	if got := len(b.subs["t"]); got != 1 {
		t.Fatalf("subscribers = %d, want 1", got)
	}
	if b.subs["t"][0] != keep {
		t.Error("unsubscribe removed the wrong channel")
	}

	b.unsubscribe("t", keep)
	if _, ok := b.subs["t"]; ok {
		t.Error("the topic was left behind with no subscribers, so a reconnect would resubscribe to it forever")
	}
}

func TestShutdownWithoutConnect(t *testing.T) {
	defer func() {
		if r := recover(); r != nil {
			t.Fatalf("Shutdown() before Connect() panicked: %v", r)
		}
	}()
	b := New()
	b.Shutdown()
	b.Shutdown()
}

func TestPublishWithoutConnect(t *testing.T) {
	if err := New().Publish("t", "1"); err == nil {
		t.Error("Publish() without a connection = nil, want an error rather than a silent success")
	}
}

// TestTLSConfig: the CA pool is the whole point of mqtt.ca-path, and an empty
// one used to be built in silence - AppendCertsFromPEM's result was discarded,
// and verification was disabled anyway.
func TestTLSConfig(t *testing.T) {
	dir := t.TempDir()

	garbage := filepath.Join(dir, "garbage.crt")
	if err := os.WriteFile(garbage, []byte("this is not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := tlsConfig(filepath.Join(dir, "missing.crt")); err == nil {
		t.Error("tlsConfig() of a missing file = nil, want an error")
	}

	_, err := tlsConfig(garbage)
	if err == nil {
		t.Fatal("tlsConfig() of a non-PEM file = nil, want an error")
	}
	if !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("tlsConfig() = %q, want it to say the file holds no certificate", err)
	}

	cfg, err := tlsConfig(testCA(t, dir))
	if err != nil {
		t.Fatal(err)
	}
	if cfg.InsecureSkipVerify {
		t.Error("InsecureSkipVerify is set: the CA pool is inert and the broker password goes to whoever answers")
	}
	if cfg.RootCAs == nil {
		t.Error("no CA pool")
	}
}

// TestClientIDIsNotTheUsername: MQTT requires a broker to disconnect an
// existing client when a second connects with the same id, so an id derived
// from the username makes two instances kick each other in a loop.
func TestClientIDIsNotTheUsername(t *testing.T) {
	id := clientID()
	if !strings.HasPrefix(id, "hal-") {
		t.Errorf("clientID() = %q, want it to identify HAL", id)
	}
	if len(id) <= len("hal-") {
		t.Errorf("clientID() = %q, want a host-specific suffix", id)
	}
}
