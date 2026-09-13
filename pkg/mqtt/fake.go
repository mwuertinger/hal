package mqtt

import (
	"fmt"
	"sync"
	"time"

	"github.com/mwuertinger/hal/pkg/config"
)

// Fake is an in-memory Broker for tests: it records what was published and
// lets a test deliver a notification as if the broker had sent one.
//
// It lives here rather than in a _test.go file because the packages that need
// it - device, frontend - are not this one.
type Fake struct {
	mu        sync.Mutex
	published []Published
	subs      map[string][]chan Notification
	closed    bool

	// PublishErr, when set, is returned by every Publish.
	PublishErr error
	// SubscribeErr, when set, is returned by every Subscribe.
	SubscribeErr error
}

// Published is one recorded call to Publish.
type Published struct {
	Topic string
	Msg   string
}

func NewFake() *Fake {
	return &Fake{subs: make(map[string][]chan Notification)}
}

func (f *Fake) Connect(config.Mqtt) error { return nil }

func (f *Fake) Publish(topic string, msg string) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.PublishErr != nil {
		return f.PublishErr
	}
	f.published = append(f.published, Published{Topic: topic, Msg: msg})
	return nil
}

func (f *Fake) Subscribe(topic string) (<-chan Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.SubscribeErr != nil {
		return nil, f.SubscribeErr
	}
	c := make(chan Notification, notificationQueue)
	f.subs[topic] = append(f.subs[topic], c)
	return c, nil
}

func (f *Fake) Shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true
	for _, channels := range f.subs {
		for _, c := range channels {
			close(c)
		}
	}
	f.subs = nil
}

// Deliver sends a notification to everything subscribed to topic, and reports
// how many subscribers received it.
func (f *Fake) Deliver(topic, msg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	n := Notification{Timestamp: time.Now(), Topic: topic, Msg: msg}
	for _, c := range f.subs[topic] {
		c <- n
	}
	return len(f.subs[topic])
}

// Published returns the calls to Publish so far, oldest first.
func (f *Fake) Published() []Published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Published(nil), f.published...)
}

// Topics returns the subscribed topics, for a test that wants to assert on
// what a device registered for.
func (f *Fake) Topics() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	topics := make([]string, 0, len(f.subs))
	for topic := range f.subs {
		topics = append(topics, topic)
	}
	return topics
}

func (f *Fake) String() string {
	return fmt.Sprintf("fake broker (%d published)", len(f.Published()))
}
