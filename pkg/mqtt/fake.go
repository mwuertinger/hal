package mqtt

import (
	"errors"
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
	onConnFns []func()
	dropped   int
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

func (f *Fake) Subscribe(topics ...string) (<-chan Notification, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if len(topics) == 0 {
		return nil, errors.New("no topics given")
	}
	if f.SubscribeErr != nil {
		// Wrapped the way the real broker wraps it, so a test asserting on the
		// message is asserting on something production would also produce.
		return nil, fmt.Errorf("subscribe to %s: %w", topics[0], f.SubscribeErr)
	}
	c := make(chan Notification, notificationQueue)
	for _, topic := range topics {
		f.subs[topic] = append(f.subs[topic], c)
	}
	return c, nil
}

func (f *Fake) OnReconnect(fn func()) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.onConnFns = append(f.onConnFns, fn)
}

// Reconnect runs the registered reconnect hooks, as the real broker does after
// re-establishing a connection.
func (f *Fake) Reconnect() {
	f.mu.Lock()
	hooks := append([]func(){}, f.onConnFns...)
	f.mu.Unlock()

	for _, hook := range hooks {
		hook()
	}
}

// Dropped reports how many notifications Deliver could not hand over.
func (f *Fake) Dropped() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.dropped
}

func (f *Fake) Shutdown() {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.closed {
		return
	}
	f.closed = true

	// Deduplicated, like the real broker's: one subscriber's topics share a
	// channel, so walking the map without this closes it once per topic - and a
	// device subscribes to two topics, so every device would panic here.
	seen := make(map[chan Notification]bool, len(f.subs))
	for _, channels := range f.subs {
		for _, c := range channels {
			if !seen[c] {
				seen[c] = true
				close(c)
			}
		}
	}
	f.subs = nil
}

// Deliver sends a notification to everything subscribed to topic, and reports
// how many subscribers received it. Like the real broker it drops rather than
// blocks, so a test with a full queue fails on the count instead of hanging.
func (f *Fake) Deliver(topic, msg string) int {
	f.mu.Lock()
	defer f.mu.Unlock()

	if f.closed {
		// The real deliver checks this too; without it a delivery after
		// Shutdown sends on a closed channel.
		return 0
	}

	n := Notification{Timestamp: time.Now(), Topic: topic, Msg: msg}
	delivered := 0
	for _, c := range f.subs[topic] {
		select {
		case c <- n:
			delivered++
		default:
			f.dropped++
		}
	}
	return delivered
}

// Published returns the calls to Publish so far, oldest first.
func (f *Fake) Published() []Published {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]Published(nil), f.published...)
}

// Channels returns the distinct subscription channels handed out, so a test
// can assert that one subscriber took one channel for all of its topics.
func (f *Fake) Channels() []chan Notification {
	f.mu.Lock()
	defer f.mu.Unlock()

	seen := make(map[chan Notification]bool)
	var out []chan Notification
	for _, channels := range f.subs {
		for _, c := range channels {
			if !seen[c] {
				seen[c] = true
				out = append(out, c)
			}
		}
	}
	return out
}

// Subscribers returns the channels registered for one topic.
func (f *Fake) Subscribers(topic string) []chan Notification {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]chan Notification(nil), f.subs[topic]...)
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
