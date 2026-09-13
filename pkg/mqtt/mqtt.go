// Package mqtt wraps an MQTT client in the small interface the rest of HAL
// needs: publish a command, and subscribe to a topic as a channel of
// notifications.
//
// The client underneath is eclipse/paho.mqtt.golang. Its predecessor here,
// yosssi/gmq, dispatched every incoming message on its own goroutine, so two
// messages that arrived in order could be delivered out of order - on a
// single-core Pi, reliably so - which is how a lamp ended up recorded in the
// state it had just left. paho serialises delivery (Order defaults to true),
// reconnects on its own, and reports a rejected CONNACK as an error rather
// than as a successful connection.
package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log"
	"os"
	"sync"
	"time"

	paho "github.com/eclipse/paho.mqtt.golang"
	"github.com/mwuertinger/hal/pkg/config"
)

const (
	// connectTimeout bounds the initial connect, so a broker that accepts the
	// TCP connection and then says nothing cannot hang startup indefinitely.
	connectTimeout = 15 * time.Second

	// publishTimeout bounds waiting for a QoS 1 PUBACK. A command that is not
	// acknowledged within it is reported as failed, which the frontend turns
	// into a switch that snaps back rather than a lie.
	publishTimeout = 5 * time.Second

	// subscribeTimeout bounds a single SUBSCRIBE round trip.
	subscribeTimeout = 10 * time.Second

	// keepAlive makes the client send PINGREQ, so a connection that dies
	// without a FIN - a rebooted router dropping conntrack state, a broker
	// host losing power - is noticed in seconds instead of in however long TCP
	// takes to give up. Without it a Publish lands in a kernel buffer for a
	// socket nobody is reading, and the command is reported as sent.
	keepAlive = 30 * time.Second

	// notificationQueue is how far one subscriber may fall behind before its
	// notifications are dropped. Deliveries must never block: paho delivers in
	// order from a single goroutine, so a consumer that stops reading would
	// otherwise stall every other subscriber too.
	notificationQueue = 64
)

type Notification struct {
	Timestamp time.Time
	Topic     string
	Msg       string
}

type Broker interface {
	Connect(mqttConfig config.Mqtt) error
	Shutdown()
	Publish(topic string, msg string) error
	Subscribe(topic string) (<-chan Notification, error)
}

type broker struct {
	client paho.Client

	// mu guards everything below it, and is also what makes delivery safe
	// against Shutdown: a message that arrives while Shutdown is running
	// blocks on the mutex and then sees closed, rather than sending on a
	// channel that is about to be closed underneath it.
	mu     sync.Mutex
	subs   map[string][]chan Notification
	closed bool
}

func New() Broker {
	return &broker{subs: make(map[string][]chan Notification)}
}

func (s *broker) Connect(mqttConfig config.Mqtt) error {
	if s.client != nil {
		return errors.New("already connected")
	}

	tlsConfig, err := tlsConfig(mqttConfig.CaPath)
	if err != nil {
		return err
	}

	opts := paho.NewClientOptions().
		AddBroker("ssl://" + mqttConfig.Server).
		SetClientID(clientID()).
		SetUsername(mqttConfig.User).
		SetPassword(mqttConfig.Password).
		SetTLSConfig(tlsConfig).
		SetConnectTimeout(connectTimeout).
		SetKeepAlive(keepAlive).
		SetPingTimeout(keepAlive / 3).
		// Every subscription is re-issued by onConnect, so there is nothing
		// worth keeping in a broker-side session. A clean session also stops a
		// second instance inheriting this one's subscriptions.
		SetCleanSession(true).
		SetAutoReconnect(true).
		SetMaxReconnectInterval(2 * time.Minute).
		// Order matters: an out-of-order pair of POWER messages is recorded as
		// the wrong state until the device next reports in.
		SetOrderMatters(true).
		SetOnConnectHandler(s.onConnect).
		SetConnectionLostHandler(func(_ paho.Client, err error) {
			log.Printf("MQTT connection lost: %v, reconnecting", err)
		}).
		SetDefaultPublishHandler(func(_ paho.Client, m paho.Message) {
			// Reachable only if the broker sends a topic nobody subscribed to.
			log.Printf("MQTT message on unrouted topic %q", m.Topic())
		})

	s.client = paho.NewClient(opts)

	token := s.client.Connect()
	if !token.WaitTimeout(connectTimeout) {
		return fmt.Errorf("connect to %s timed out after %v", mqttConfig.Server, connectTimeout)
	}
	if err := token.Error(); err != nil {
		// Unlike the client this replaced, a refused CONNACK - a wrong
		// password, say - arrives here as an error instead of being reported
		// as a successful connection.
		return fmt.Errorf("connect to %s failed: %w", mqttConfig.Server, err)
	}

	log.Println("MQTT connection established")

	return nil
}

// tlsConfig builds the TLS configuration for the broker connection. The CA
// pool is the whole point of mqtt.ca-path, and verification against it is on:
// this connection carries the broker password, and the LAN it crosses is
// exactly where an attacker able to answer for the broker's address would be.
func tlsConfig(caPath string) (*tls.Config, error) {
	caCert, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("unable to read mqtt.ca-path: %v", err)
	}

	pool := x509.NewCertPool()
	// AppendCertsFromPEM reports whether it found anything. Ignoring it leaves
	// an empty pool, which fails later as an opaque handshake error rather
	// than as "your CA file has no certificate in it".
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("mqtt.ca-path %q contains no PEM certificate", caPath)
	}

	return &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}, nil
}

// clientID identifies this instance to the broker. It must not be the username:
// MQTT requires a broker to disconnect an existing client when a second one
// connects with the same id, so two instances sharing a username kick each
// other in a loop, and the id also shows up in broker logs and ACL patterns.
func clientID() string {
	host, err := os.Hostname()
	if err != nil || host == "" {
		host = fmt.Sprintf("%d", os.Getpid())
	}
	return "hal-" + host
}

// onConnect runs after every successful connect, the automatic reconnects
// included. Subscriptions do not survive a reconnect - the broker was told the
// session is clean, and paho's own routes only cover message dispatch - so
// this is what stops a reconnected HAL from sitting there receiving nothing.
func (s *broker) onConnect(client paho.Client) {
	s.mu.Lock()
	topics := make([]string, 0, len(s.subs))
	for topic := range s.subs {
		topics = append(topics, topic)
	}
	closed := s.closed
	s.mu.Unlock()

	if closed {
		return
	}

	for _, topic := range topics {
		if err := s.subscribe(client, topic); err != nil {
			// Nothing here can recover: a failed re-subscribe means this topic
			// is dark until the next reconnect. Say so loudly.
			log.Printf("MQTT resubscribe to %q failed: %v", topic, err)
		}
	}

	if len(topics) > 0 {
		log.Printf("MQTT subscribed to %d topic(s)", len(topics))
	}
}

// subscribe issues one SUBSCRIBE and routes its messages to every channel
// registered for the topic.
func (s *broker) subscribe(client paho.Client, topic string) error {
	token := client.Subscribe(topic, 1, func(_ paho.Client, m paho.Message) {
		s.deliver(m.Topic(), string(m.Payload()))
	})
	if !token.WaitTimeout(subscribeTimeout) {
		return fmt.Errorf("timed out after %v", subscribeTimeout)
	}
	return token.Error()
}

// deliver fans one message out to the subscribers of its topic. It never
// blocks: paho calls it from the single goroutine that preserves message
// order, so waiting for one slow consumer would hold up every other one.
func (s *broker) deliver(topic, payload string) {
	notification := Notification{Timestamp: time.Now(), Topic: topic, Msg: payload}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	for _, c := range s.subs[topic] {
		select {
		case c <- notification:
		default:
			log.Printf("MQTT %s: subscriber %d notifications behind, dropping", topic, notificationQueue)
		}
	}
}

func (s *broker) Shutdown() {
	if s.client != nil {
		// Disconnect stops the reader, so no further deliver call can start
		// after it returns; one already inside deliver is holding mu, which
		// the lock below waits for.
		s.client.Disconnect(250)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true
	for _, channels := range s.subs {
		for _, c := range channels {
			close(c)
		}
	}
	s.subs = nil

	log.Printf("broker: shutdown complete")
}

func (s *broker) Publish(topic string, msg string) error {
	if s.client == nil {
		return errors.New("not connected")
	}
	if !s.client.IsConnected() {
		// Without this, paho queues the message for the next connection and
		// reports success, so a command sent while the broker is down looks
		// like it reached the lamp.
		return errors.New("broker not connected")
	}

	// QoS 1: the command must arrive, and a lamp switched twice is switched
	// to the same state, so at-least-once costs nothing here.
	token := s.client.Publish(topic, 1, false, msg)
	if !token.WaitTimeout(publishTimeout) {
		return fmt.Errorf("publish to %s timed out after %v", topic, publishTimeout)
	}
	return token.Error()
}

func (s *broker) Subscribe(topic string) (<-chan Notification, error) {
	if s.client == nil {
		return nil, errors.New("not connected")
	}

	c := make(chan Notification, notificationQueue)

	s.mu.Lock()
	if s.closed {
		s.mu.Unlock()
		return nil, errors.New("broker is shut down")
	}
	first := len(s.subs[topic]) == 0
	s.subs[topic] = append(s.subs[topic], c)
	s.mu.Unlock()

	if !first {
		// Already subscribed at the broker; deliver fans out to both channels.
		return c, nil
	}

	if err := s.subscribe(s.client, topic); err != nil {
		s.unsubscribe(topic, c)
		return nil, fmt.Errorf("subscribe to %s: %w", topic, err)
	}

	return c, nil
}

// unsubscribe removes one channel again, so a failed Subscribe does not leave
// a subscriber behind that onConnect would then try to re-establish forever.
func (s *broker) unsubscribe(topic string, c chan Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()

	remaining := s.subs[topic][:0]
	for _, existing := range s.subs[topic] {
		if existing != c {
			remaining = append(remaining, existing)
		}
	}
	if len(remaining) == 0 {
		delete(s.subs, topic)
	} else {
		s.subs[topic] = remaining
	}
}
