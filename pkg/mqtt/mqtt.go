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

	// Subscribe delivers every message on any of topics to one channel. The
	// topics of one subscriber share a channel deliberately: messages are
	// delivered in order, and two channels read by a select would put that
	// order back at the mercy of the scheduler.
	Subscribe(topics ...string) (<-chan Notification, error)

	// OnReconnect registers f to run after the connection has been
	// re-established and the subscriptions re-issued. Nothing is retained
	// while the connection is down, so a subscriber that needs the current
	// state of the world has to ask for it again here.
	OnReconnect(f func())
}

// ErrConfig marks a failure that no amount of retrying will fix - an
// unreadable CA file, a CA file with no certificate in it. main turns it into
// exit 78, which hal.service names in RestartPreventExitStatus= so the unit
// fails visibly instead of restarting every five seconds forever.
var ErrConfig = errors.New("configuration error")

type broker struct {
	client paho.Client

	// mu guards everything below it, and is also what makes delivery safe
	// against Shutdown: a message that arrives while Shutdown is running
	// blocks on the mutex and then sees closed, rather than sending on a
	// channel that is about to be closed underneath it.
	mu        sync.Mutex
	subs      map[string][]chan Notification
	onConnFns []func()
	dropped   map[string]int
	// generation counts connections, so a retry started for one of them can
	// tell that it has been superseded.
	generation uint64
	closed     bool

	// done is closed by Shutdown, which is what ends a resubscribe retry.
	done chan struct{}
}

func New() Broker {
	return &broker{
		subs:    make(map[string][]chan Notification),
		dropped: make(map[string]int),
		done:    make(chan struct{}),
	}
}

func (s *broker) Connect(mqttConfig config.Mqtt) error {
	if s.client != nil {
		return errors.New("already connected")
	}

	tlsConfig, err := tlsConfig(mqttConfig.CaPath, mqttConfig.SkipHostnameVerify)
	if err != nil {
		return err
	}
	if mqttConfig.SkipHostnameVerify {
		log.Printf("MQTT: mqtt.skip-hostname-verify is set - the broker's certificate chain is "+
			"verified against %s, but not that it names %s", mqttConfig.CaPath, mqttConfig.Server)
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

	client := paho.NewClient(opts)
	s.client = client

	token := client.Connect()
	if !token.WaitTimeout(connectTimeout) {
		// Cleared, so a caller that decides to retry is not told it is already
		// connected to a client that never connected - and disconnected first,
		// because paho's attempt is still running and may yet succeed, which
		// would leave a live authenticated client that Shutdown skips.
		client.Disconnect(0)
		s.client = nil
		return fmt.Errorf("connect to %s timed out after %v", mqttConfig.Server, connectTimeout)
	}
	if err := token.Error(); err != nil {
		client.Disconnect(0)
		s.client = nil
		if hostnameProblem(err) {
			// The one certificate failure with a documented way forward, and
			// the operator has no way to guess the key from the error alone.
			return fmt.Errorf("%w: connect to %s failed: %w (if the broker's certificate has no "+
				"subjectAltName, see mqtt.skip-hostname-verify)", ErrConfig, mqttConfig.Server, err)
		}
		if certificateProblem(err) {
			// A certificate that does not chain to mqtt.ca-path, or whose SAN
			// does not cover mqtt.server, fails identically on every retry.
			// Restarting every five seconds forever hides that; failing the
			// unit puts it in systemctl status.
			return fmt.Errorf("%w: connect to %s failed: %w", ErrConfig, mqttConfig.Server, err)
		}
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
func tlsConfig(caPath string, skipHostnameVerify bool) (*tls.Config, error) {
	caCert, err := os.ReadFile(caPath)
	if err != nil {
		return nil, fmt.Errorf("%w: unable to read mqtt.ca-path: %w", ErrConfig, err)
	}

	pool := x509.NewCertPool()
	// AppendCertsFromPEM reports whether it found anything. Ignoring it leaves
	// an empty pool, which fails later as an opaque handshake error rather
	// than as "your CA file has no certificate in it".
	if !pool.AppendCertsFromPEM(caCert) {
		return nil, fmt.Errorf("%w: mqtt.ca-path %q contains no PEM certificate", ErrConfig, caPath)
	}

	config := &tls.Config{RootCAs: pool, MinVersion: tls.VersionTLS12}
	if !skipHostnameVerify {
		return config, nil
	}

	// Go has ignored the Common Name field since 1.15, so a certificate with no
	// subjectAltName cannot be matched against any name - which is the state of
	// more than one long-lived broker certificate issued before that was
	// enforced. Turning the standard verification off is the only way to reach
	// the handshake at all, so the chain check is done here instead of being
	// given up with it: InsecureSkipVerify disables Go's checks, and
	// VerifyPeerCertificate puts the part that matters back.
	//
	// What is actually lost is the binding between the certificate and this
	// host. A certificate signed by the configured CA for some other name is
	// accepted here, where it would otherwise be refused.
	config.InsecureSkipVerify = true
	config.VerifyPeerCertificate = func(rawCerts [][]byte, _ [][]*x509.Certificate) error {
		if len(rawCerts) == 0 {
			return errors.New("the broker presented no certificate")
		}

		certs := make([]*x509.Certificate, 0, len(rawCerts))
		for _, raw := range rawCerts {
			cert, err := x509.ParseCertificate(raw)
			if err != nil {
				return fmt.Errorf("parsing the broker certificate: %w", err)
			}
			certs = append(certs, cert)
		}

		intermediates := x509.NewCertPool()
		for _, cert := range certs[1:] {
			intermediates.AddCert(cert)
		}

		_, err := certs[0].Verify(x509.VerifyOptions{
			Roots:         pool,
			Intermediates: intermediates,
			KeyUsages:     []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		})
		return err
	}

	return config, nil
}

// certificateProblem reports whether err is the broker's certificate being
// wrong in a way that will still be wrong on the next attempt - as opposed to
// the broker being unreachable, or the clock being behind.
//
// The order matters. x509 reports "has expired" and "is not yet valid" with the
// same Expired reason, and this runs on a Raspberry Pi, which has no real-time
// clock: on a cold boot HAL can reach the broker before timesyncd has corrected
// the date, and a certificate issued today then reads as not yet valid.
// Retrying fixes that in seconds. Exit 78 does not - it stops the unit, and the
// house stays dark until somebody logs in. So the validity window is checked
// first and excluded, before the catch-all below, which would otherwise match
// it right back.
func certificateProblem(err error) bool {
	var invalid x509.CertificateInvalidError
	if errors.As(err, &invalid) {
		return invalid.Reason != x509.Expired
	}

	var unknownAuthority x509.UnknownAuthorityError
	var hostname x509.HostnameError
	return errors.As(err, &unknownAuthority) || errors.As(err, &hostname)
}

// hostnameProblem reports whether err is specifically the certificate not
// naming the host it was fetched from.
func hostnameProblem(err error) bool {
	var hostname x509.HostnameError
	return errors.As(err, &hostname)
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
	s.generation++
	generation := s.generation
	topics := make([]string, 0, len(s.subs))
	for topic := range s.subs {
		topics = append(topics, topic)
	}
	hooks := append([]func(){}, s.onConnFns...)
	closed := s.closed
	s.mu.Unlock()

	if closed {
		return
	}

	var failed []string
	for _, topic := range topics {
		if err := s.subscribe(client, topic); err != nil {
			log.Printf("MQTT subscribe to %q failed: %v", topic, err)
			failed = append(failed, topic)
		}
	}

	if len(failed) > 0 {
		// A topic that stays unsubscribed is a device that silently never
		// updates again, so keep trying rather than leaving one log line
		// behind. The retry ends when this connection ends, at which point the
		// next onConnect takes over - generation is what tells it apart from
		// the connection that replaced it, since paho reuses one Client and
		// IsConnectionOpen would be true again for the newer one.
		go s.retrySubscribes(client, generation, failed)
	}

	if len(topics) > 0 {
		log.Printf("MQTT subscribed to %d of %d topic(s)", len(topics)-len(failed), len(topics))
	}

	// Hooks run last and off this goroutine: they publish, and paho calls
	// onConnect from a path that should not be waiting on a PUBACK.
	if len(hooks) > 0 {
		go func() {
			for _, hook := range hooks {
				hook()
			}
		}()
	}
}

// retrySubscribes keeps trying the topics onConnect could not subscribe to,
// for as long as this connection lasts.
func (s *broker) retrySubscribes(client paho.Client, generation uint64, topics []string) {
	delay := 5 * time.Second

	for {
		select {
		case <-s.done:
			return
		case <-time.After(delay):
		}

		s.mu.Lock()
		current := s.generation
		s.mu.Unlock()

		if !client.IsConnectionOpen() || current != generation {
			// This connection is gone; the onConnect for its replacement
			// re-issues everything anyway.
			return
		}

		var failed []string
		for _, topic := range topics {
			if err := s.subscribe(client, topic); err != nil {
				failed = append(failed, topic)
			} else {
				log.Printf("MQTT subscribe to %q recovered", topic)
			}
		}
		if len(failed) == 0 {
			return
		}
		topics = failed

		if delay < time.Minute {
			delay *= 2
		}
		log.Printf("MQTT still cannot subscribe to %v, retrying in %v", topics, delay)
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
	if err := token.Error(); err != nil {
		return err
	}

	// A broker that refuses a subscription - an ACL that does not grant the
	// topic - answers SUBACK with 0x80, which paho records as the granted QoS
	// and does not treat as an error. Unchecked, the device is registered,
	// shown in the UI, and reports off forever.
	if subToken, ok := token.(*paho.SubscribeToken); ok {
		if qos, granted := subToken.Result()[topic]; granted && qos == 0x80 {
			return fmt.Errorf("the broker refused the subscription (SUBACK 0x80); check its ACL")
		}
	}

	return nil
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
			// Counted rather than logged per message: a subscriber that has
			// stopped reading would otherwise write to the SD card as fast as
			// the broker can deliver.
			s.dropped[topic]++
			if n := s.dropped[topic]; n == 1 || n%100 == 0 {
				log.Printf("MQTT %s: subscriber %d notifications behind, %d dropped so far",
					topic, cap(c), n)
			}
		}
	}
}

func (s *broker) OnReconnect(f func()) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.onConnFns = append(s.onConnFns, f)
}

func (s *broker) Shutdown() {
	if s.client != nil {
		// Disconnect is a deadline rather than a join - it returns after the
		// quiesce period whether or not the teardown finished - so it is the
		// mutex below, not this call, that makes a delivery in flight safe.
		s.client.Disconnect(250)
	}

	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return
	}
	s.closed = true
	close(s.done)

	// Deduplicated: one subscriber's topics share a channel, so walking the
	// map without this closes that channel once per topic.
	seen := make(map[chan Notification]bool, len(s.subs))
	for _, channels := range s.subs {
		for _, c := range channels {
			if !seen[c] {
				seen[c] = true
				close(c)
			}
		}
	}
	s.subs = nil

	log.Printf("broker: shutdown complete")
}

func (s *broker) Publish(topic string, msg string) error {
	if s.client == nil {
		return errors.New("not connected")
	}
	// IsConnectionOpen, not IsConnected: with AutoReconnect set the latter
	// reports true for the whole reconnect window, and a QoS 1 publish in that
	// state is stored rather than sent - the token never completes, the caller
	// waits out publishTimeout and is told the command failed, and then paho
	// replays the stored message when the connection returns. A lamp switching
	// itself on minutes after the user was told the switch failed is worse
	// than a switch that plainly did not work.
	if !s.client.IsConnectionOpen() {
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

// Subscribe registers one channel for every topic given. One channel rather
// than one per topic: paho delivers in order, and a consumer that had to
// select between two channels would lose that order - a tele/<id>/STATE frame
// restating the old state could then be applied after the stat/<id>/POWER that
// superseded it, leaving the device recorded in the state it just left.
func (s *broker) Subscribe(topics ...string) (<-chan Notification, error) {
	if s.client == nil {
		return nil, errors.New("not connected")
	}
	if len(topics) == 0 {
		return nil, errors.New("no topics given")
	}

	c := make(chan Notification, notificationQueue)

	fresh, err := s.register(topics, c)
	if err != nil {
		return nil, err
	}

	for _, topic := range fresh {
		if err := s.subscribe(s.client, topic); err != nil {
			s.unsubscribe(topics, c)
			return nil, fmt.Errorf("subscribe to %s: %w", topic, err)
		}
	}

	return c, nil
}

// register adds c to the subscriber list of every topic and reports which of
// them nobody was subscribed to yet.
//
// Split out so the locked section can use defer. Subscribe does its broker I/O
// outside the lock, so it cannot simply defer the unlock itself - and an
// explicit Unlock is not equivalent: a panic in here would otherwise leave the
// broker mutex held for the life of the process, which wedges delivery,
// shutdown and every later subscription while the daemon carries on looking
// healthy.
func (s *broker) register(topics []string, c chan Notification) ([]string, error) {
	s.mu.Lock()
	defer s.mu.Unlock()

	if s.closed {
		return nil, errors.New("broker is shut down")
	}

	fresh := make([]string, 0, len(topics))
	seen := make(map[string]bool, len(topics))
	for _, topic := range topics {
		if seen[topic] {
			// Registering twice would deliver every message twice.
			continue
		}
		seen[topic] = true

		if len(s.subs[topic]) == 0 {
			fresh = append(fresh, topic)
		}
		s.subs[topic] = append(s.subs[topic], c)
	}
	return fresh, nil
}

// unsubscribe removes one channel again, so a failed Subscribe does not leave
// a subscriber behind that onConnect would then try to re-establish forever.
func (s *broker) unsubscribe(topics []string, c chan Notification) {
	s.mu.Lock()
	defer s.mu.Unlock()

	for _, topic := range topics {
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
}
