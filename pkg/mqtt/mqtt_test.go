package mqtt

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/pem"
	"errors"
	"fmt"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/mwuertinger/hal/pkg/config"
)

// TestDeliverDuringShutdown is the regression test for the shutdown panic: the
// broker used to close the subscriber channels while a message handler was
// parked on a send into one, which is "send on closed channel" - unrecoverable,
// and enough to mark a clean systemctl stop as failed.
func TestDeliverDuringShutdown(t *testing.T) {
	for i := 0; i < 50; i++ {
		b := New().(*broker)
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
	b := New().(*broker)
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
	b := New().(*broker)
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
	b := New().(*broker)
	keep := make(chan Notification, 1)
	drop := make(chan Notification, 1)
	b.subs["t"] = []chan Notification{keep, drop}

	b.unsubscribe([]string{"t"}, drop)

	if got := len(b.subs["t"]); got != 1 {
		t.Fatalf("subscribers = %d, want 1", got)
	}
	if b.subs["t"][0] != keep {
		t.Error("unsubscribe removed the wrong channel")
	}

	b.unsubscribe([]string{"t"}, keep)
	if _, ok := b.subs["t"]; ok {
		t.Error("the topic was left behind with no subscribers, so a reconnect would resubscribe to it forever")
	}
}

func TestOnReconnectHooksRun(t *testing.T) {
	f := NewFake()
	var ran int
	f.OnReconnect(func() { ran++ })
	f.Reconnect()
	if ran != 1 {
		t.Errorf("hook ran %d times, want 1", ran)
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

	if _, err := tlsConfig(filepath.Join(dir, "missing.crt"), false); err == nil {
		t.Error("tlsConfig() of a missing file = nil, want an error")
	}

	_, err := tlsConfig(garbage, false)
	if err == nil {
		t.Fatal("tlsConfig() of a non-PEM file = nil, want an error")
	}
	if !strings.Contains(err.Error(), "no PEM certificate") {
		t.Errorf("tlsConfig() = %q, want it to say the file holds no certificate", err)
	}

	cfg, err := tlsConfig(testCA(t, dir), false)
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

// --- against a real MQTT handshake ------------------------------------------
//
// Everything below talks to testBroker over TLS, because it covers what the
// client does on the wire, which no amount of testing this package's own maps
// can reach.

func connected(t *testing.T) (*broker, *testBroker) {
	t.Helper()

	tb, caPath := startTestBroker(t)
	b := New().(*broker)
	t.Cleanup(b.Shutdown)

	if err := b.Connect(config.Mqtt{Server: tb.Addr(), CaPath: caPath, User: "hal", Password: "x"}); err != nil {
		t.Fatalf("connect: %v", err)
	}
	return b, tb
}

func TestConnectAndSubscribe(t *testing.T) {
	b, tb := connected(t)

	c, err := b.Subscribe("stat/lamp/POWER", "tele/lamp/STATE")
	if err != nil {
		t.Fatal(err)
	}
	if got := len(tb.Topics()); got != 2 {
		t.Errorf("broker sees %d topics, want 2: %v", got, tb.Topics())
	}

	tb.Publish("stat/lamp/POWER", "ON")
	select {
	case n := <-c:
		if n.Msg != "ON" || n.Topic != "stat/lamp/POWER" {
			t.Errorf("got %+v", n)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}
}

// TestSubscribedTopicsShareOneChannelInOrder: the two topics of one device
// contradict each other by design - a tele/<id>/STATE frame published just
// before a command reached the lamp restates the state being switched away
// from - so they must arrive in the order the broker sent them. One channel is
// what guarantees that; two channels read by a select would not.
func TestSubscribedTopicsShareOneChannelInOrder(t *testing.T) {
	b, tb := connected(t)

	c, err := b.Subscribe("stat/lamp/POWER", "tele/lamp/STATE")
	if err != nil {
		t.Fatal(err)
	}

	want := []string{
		"tele/lamp/STATE", "stat/lamp/POWER", "stat/lamp/POWER", "tele/lamp/STATE", "stat/lamp/POWER",
	}
	for i, topic := range want {
		tb.Publish(topic, fmt.Sprintf("m%d", i))
	}

	for i, topic := range want {
		select {
		case n := <-c:
			if n.Topic != topic || n.Msg != fmt.Sprintf("m%d", i) {
				t.Fatalf("message %d = %s %q, want %s m%d", i, n.Topic, n.Msg, topic, i)
			}
		case <-time.After(5 * time.Second):
			t.Fatalf("message %d never arrived", i)
		}
	}
}

// TestSubscriptionsSurviveAReconnect is the central claim of the migration:
// the client this replaced could not reconnect at all, and a client that
// reconnects without re-subscribing is worse than one that dies, because the
// process stays up receiving nothing.
func TestSubscriptionsSurviveAReconnect(t *testing.T) {
	b, tb := connected(t)

	c, err := b.Subscribe("stat/lamp/POWER")
	if err != nil {
		t.Fatal(err)
	}

	tb.DropAll()

	deadline := time.Now().Add(30 * time.Second)
	for tb.Connects() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if tb.Connects() < 2 {
		t.Fatal("the client never reconnected")
	}

	for time.Now().Before(deadline) {
		if tb.Publish("stat/lamp/POWER", "OFF") > 0 {
			break
		}
		time.Sleep(20 * time.Millisecond)
	}

	select {
	case n := <-c:
		if n.Msg != "OFF" {
			t.Errorf("Msg = %q, want OFF", n.Msg)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("nothing arrived after the reconnect: the subscription was lost")
	}
}

// TestOnReconnectRunsAfterAReconnect: nothing is retained while the connection
// is down, so a subscriber that needs the current state of the world has to be
// told it can ask again.
func TestOnReconnectRunsAfterAReconnect(t *testing.T) {
	b, tb := connected(t)

	if _, err := b.Subscribe("stat/lamp/POWER"); err != nil {
		t.Fatal(err)
	}

	ran := make(chan struct{}, 4)
	b.OnReconnect(func() { ran <- struct{}{} })

	tb.DropAll()

	select {
	case <-ran:
	case <-time.After(30 * time.Second):
		t.Fatal("the reconnect hook never ran")
	}
}

// TestSubscribeRejectedByTheBroker: a broker that refuses a subscription
// answers SUBACK 0x80, which paho records as the granted QoS rather than as an
// error. Unchecked, the device is registered, shown in the UI, and reports off
// forever - the exact failure the error propagation was added to prevent.
func TestSubscribeRejectedByTheBroker(t *testing.T) {
	b, tb := connected(t)
	tb.RefuseSubscriptions(true)

	_, err := b.Subscribe("stat/lamp/POWER")
	if err == nil {
		t.Fatal("Subscribe() = nil, want an error")
	}
	if !strings.Contains(err.Error(), "0x80") {
		t.Errorf("Subscribe() = %q, want it to name the refusal", err)
	}
}

// TestPublishWhileReconnectingFails, and does not deliver later. paho's
// IsConnected() reports true for the whole reconnect window when AutoReconnect
// is set, and a QoS 1 publish in that state is stored, not sent: the token
// never completes, the caller waits out publishTimeout and is told the command
// failed - and then the message is replayed when the connection returns. A lamp
// that switches itself on minutes after the user was told the switch failed is
// worse than one that plainly did not switch.
func TestPublishWhileReconnectingFails(t *testing.T) {
	b, tb := connected(t)

	if _, err := b.Subscribe("stat/lamp/POWER"); err != nil {
		t.Fatal(err)
	}

	tb.RefuseConnections(true)
	tb.DropAll()

	deadline := time.Now().Add(10 * time.Second)
	for b.client.IsConnectionOpen() && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}

	start := time.Now()
	err := b.Publish("cmnd/lamp/POWER", "1")
	elapsed := time.Since(start)

	if err == nil {
		t.Fatal("Publish() while reconnecting = nil, want an error")
	}
	if elapsed > publishTimeout {
		t.Errorf("Publish() took %v: it waited out the timeout instead of failing immediately", elapsed)
	}

	// And the command must not turn up later.
	tb.RefuseConnections(false)
	for time.Now().Before(deadline) && tb.Connects() < 2 {
		time.Sleep(20 * time.Millisecond)
	}
	time.Sleep(500 * time.Millisecond)

	for _, p := range tb.Published() {
		if p.Topic == "cmnd/lamp/POWER" {
			t.Errorf("the failed command was replayed after the reconnect: %+v", p)
		}
	}
}

func TestPublishReachesTheBroker(t *testing.T) {
	b, tb := connected(t)

	if err := b.Publish("cmnd/lamp/POWER", "1"); err != nil {
		t.Fatal(err)
	}

	published := tb.Published()
	if len(published) != 1 || published[0].Topic != "cmnd/lamp/POWER" || published[0].Msg != "1" {
		t.Errorf("broker received %+v, want one cmnd/lamp/POWER=1", published)
	}
}

// TestConnectRefusedCredentials: the client this replaced could not see a
// rejected CONNACK at all, so a wrong password logged "MQTT connection
// established" and then crash-looped with an error about already being
// connected.
func TestConnectRefusedCredentials(t *testing.T) {
	tb, caPath := startTestBroker(t)
	tb.ConnackCode = 5 // not authorized

	b := New().(*broker)
	err := b.Connect(config.Mqtt{Server: tb.Addr(), CaPath: caPath, User: "hal", Password: "wrong"})
	if err == nil {
		t.Fatal("Connect() with refused credentials = nil, want an error")
	}
	if b.client != nil {
		t.Error("the client was left set after a failed connect, so a retry reports 'already connected'")
	}
}

// TestConnectRejectsAnUntrustedCertificate: this connection carries the broker
// password across the LAN, which is exactly where anyone able to answer for the
// broker's address would be.
func TestConnectRejectsAnUntrustedCertificate(t *testing.T) {
	tb, _ := startTestBroker(t)
	_, otherCA := serverCert(t, t.TempDir()) // a CA that signed nothing here

	err := New().Connect(config.Mqtt{Server: tb.Addr(), CaPath: otherCA, User: "hal"})
	if err == nil {
		t.Fatal("Connect() to a server the configured CA does not vouch for = nil, want an error")
	}
	if !strings.Contains(err.Error(), "certificate") {
		t.Errorf("Connect() = %q, want a certificate error", err)
	}
}

func TestConnectConfigErrorsAreMarked(t *testing.T) {
	if err := New().Connect(config.Mqtt{Server: "127.0.0.1:1", CaPath: "/nonexistent/ca.pem"}); !errors.Is(err, ErrConfig) {
		t.Errorf("Connect() with an unreadable CA = %v, want it to wrap ErrConfig so main can exit 78", err)
	}

	dir := t.TempDir()
	empty := filepath.Join(dir, "empty.pem")
	if err := os.WriteFile(empty, []byte("not a certificate"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := New().Connect(config.Mqtt{Server: "127.0.0.1:1", CaPath: empty}); !errors.Is(err, ErrConfig) {
		t.Errorf("Connect() with a CA file holding no certificate = %v, want ErrConfig", err)
	}
}

// TestSubscribeAfterShutdown and TestNoResubscribeAfterShutdown cover the two
// reachable effects of the closed flag. Both were previously invisible to the
// suite: the flag could be deleted from Subscribe and from onConnect and
// everything stayed green, because Shutdown also nils the map.
func TestSubscribeAfterShutdown(t *testing.T) {
	b, _ := connected(t)
	b.Shutdown()

	if _, err := b.Subscribe("stat/lamp/POWER"); err == nil {
		t.Error("Subscribe() after Shutdown() = nil, want an error")
	}
}

// TestNoResubscribeAfterShutdown: Disconnect returns after its quiesce period
// whether or not the teardown finished, so paho can still be reconnecting when
// Shutdown returns. Without the guard, that reconnect's onConnect re-subscribes
// to topics nobody is listening to any more.
func TestNoResubscribeAfterShutdown(t *testing.T) {
	b, tb := connected(t)

	if _, err := b.Subscribe("stat/lamp/POWER"); err != nil {
		t.Fatal(err)
	}

	b.mu.Lock()
	b.closed = true
	b.mu.Unlock()

	// Drive onConnect directly: this is the callback paho would run on a
	// reconnect that lands inside the Disconnect window.
	before := len(tb.Topics())
	b.subs = map[string][]chan Notification{"tele/other/STATE": nil}
	b.onConnect(b.client)

	if got := len(tb.Topics()); got != before {
		t.Errorf("the broker gained %d topic(s) after shutdown, want none", got-before)
	}
}

// TestResubscribeRetryRecovers covers the retry loop: a topic the broker
// refuses at reconnect time must not stay dark until the next disconnect,
// which for a healthy connection could be days.
func TestResubscribeRetryRecovers(t *testing.T) {
	b, tb := connected(t)

	c, err := b.Subscribe("stat/lamp/POWER")
	if err != nil {
		t.Fatal(err)
	}

	// The ACL turns hostile, then the connection drops, so onConnect's
	// resubscribe fails and hands over to the retry.
	tb.RefuseSubscriptions(true)
	tb.DropAll()

	deadline := time.Now().Add(30 * time.Second)
	for tb.Connects() < 2 && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if tb.Connects() < 2 {
		t.Fatal("the client never reconnected")
	}

	// The ACL is fixed. Nothing else will prompt another SUBSCRIBE.
	tb.RefuseSubscriptions(false)

	for time.Now().Before(deadline) {
		if tb.Publish("stat/lamp/POWER", "OFF") > 0 {
			break
		}
		time.Sleep(100 * time.Millisecond)
	}

	select {
	case n := <-c:
		if n.Msg != "OFF" {
			t.Errorf("Msg = %q, want OFF", n.Msg)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("the topic stayed dark: the resubscribe was never retried")
	}
}

func TestCertificateProblemIsAConfigError(t *testing.T) {
	tb, _ := startTestBroker(t)
	_, otherCA := serverCert(t, t.TempDir())

	err := New().Connect(config.Mqtt{Server: tb.Addr(), CaPath: otherCA, User: "hal"})
	if !errors.Is(err, ErrConfig) {
		t.Errorf("Connect() with an untrusted certificate = %v, want it to wrap ErrConfig: it fails "+
			"the same way on every retry, so the unit should fail rather than restart forever", err)
	}
}

func TestSubscribeIgnoresARepeatedTopic(t *testing.T) {
	b, tb := connected(t)

	c, err := b.Subscribe("stat/lamp/POWER", "stat/lamp/POWER")
	if err != nil {
		t.Fatal(err)
	}

	tb.Publish("stat/lamp/POWER", "ON")

	select {
	case <-c:
	case <-time.After(5 * time.Second):
		t.Fatal("no notification")
	}

	select {
	case n := <-c:
		t.Errorf("the message was delivered twice: %+v", n)
	case <-time.After(200 * time.Millisecond):
	}
}

// TestSkipHostnameVerify covers the escape hatch for a broker certificate with
// no subjectAltName, and - more importantly - that it still refuses a
// certificate the configured CA did not sign. Skipping the name check must not
// become skipping verification, which is what the code this replaced did.
func TestSkipHostnameVerify(t *testing.T) {
	t.Run("a certificate without a SAN is refused by default", func(t *testing.T) {
		tb, caPath := startTestBrokerOpts(t, false)

		err := New().Connect(config.Mqtt{Server: tb.Addr(), CaPath: caPath, User: "hal"})
		if err == nil {
			t.Fatal("Connect() = nil, want a certificate error")
		}
		if !errors.Is(err, ErrConfig) {
			t.Errorf("Connect() = %v, want it to wrap ErrConfig", err)
		}
	})

	t.Run("and accepted with skip-hostname-verify", func(t *testing.T) {
		tb, caPath := startTestBrokerOpts(t, false)

		b := New()
		t.Cleanup(b.Shutdown)
		if err := b.Connect(config.Mqtt{
			Server: tb.Addr(), CaPath: caPath, User: "hal", SkipHostnameVerify: true,
		}); err != nil {
			t.Fatalf("Connect() with skip-hostname-verify: %v", err)
		}
		if tb.Connects() != 1 {
			t.Errorf("broker saw %d connects, want 1", tb.Connects())
		}
	})

	t.Run("but a certificate from another CA is still refused", func(t *testing.T) {
		tb, _ := startTestBrokerOpts(t, false)
		_, otherCA := serverCert(t, t.TempDir()) // a CA that signed nothing here

		err := New().Connect(config.Mqtt{
			Server: tb.Addr(), CaPath: otherCA, User: "hal", SkipHostnameVerify: true,
		})
		if err == nil {
			t.Fatal("Connect() = nil: skipping the hostname check must not skip the chain check")
		}
		if !strings.Contains(err.Error(), "authority") && !strings.Contains(err.Error(), "certificate") {
			t.Errorf("Connect() = %q, want a chain verification error", err)
		}
	})
}

// TestClockSkewIsNotAConfigError: x509 reports "has expired" and "is not yet
// valid" with the same reason, and this runs on a Raspberry Pi, which has no
// real-time clock. HAL can reach the broker before timesyncd has corrected the
// date, at which point a certificate issued today reads as not yet valid.
// Classifying that as a configuration error stops the unit - and the house
// stays dark until somebody logs in - where retrying would have fixed it in
// seconds.
func TestClockSkewIsNotAConfigError(t *testing.T) {
	for _, tc := range []struct {
		name                string
		notBefore, notAfter time.Time
	}{
		{"not yet valid", time.Now().Add(24 * time.Hour), time.Now().Add(48 * time.Hour)},
		{"expired", time.Now().Add(-48 * time.Hour), time.Now().Add(-24 * time.Hour)},
	} {
		for _, skip := range []bool{false, true} {
			name := tc.name
			if skip {
				name += ", skip-hostname-verify"
			}
			t.Run(name, func(t *testing.T) {
				dir := t.TempDir()
				cert, caPath := serverCertAt(t, dir, true, tc.notBefore, tc.notAfter)

				raw, err := net.Listen("tcp", "127.0.0.1:0")
				if err != nil {
					t.Fatal(err)
				}
				ln := tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
				tb := serveTestBroker(ln)
				t.Cleanup(func() { tb.Close() })

				err = New().Connect(config.Mqtt{
					Server: tb.Addr(), CaPath: caPath, User: "hal", SkipHostnameVerify: skip,
				})
				if err == nil {
					t.Fatal("Connect() = nil, want the handshake to fail")
				}
				if errors.Is(err, ErrConfig) {
					t.Errorf("Connect() = %v\nclassified as a permanent configuration error, so the unit "+
						"stops; a clock that has not caught up fixes itself on the next attempt", err)
				}
			})
		}
	}
}

// TestSANlessCertificateNamesTheEscapeHatch: the flag is the only way forward
// from this failure, and an operator cannot guess the key from the error.
func TestSANlessCertificateNamesTheEscapeHatch(t *testing.T) {
	tb, caPath := startTestBrokerOpts(t, false)

	err := New().Connect(config.Mqtt{Server: tb.Addr(), CaPath: caPath, User: "hal"})
	if err == nil {
		t.Fatal("Connect() = nil, want a certificate error")
	}
	if !strings.Contains(err.Error(), "skip-hostname-verify") {
		t.Errorf("Connect() = %q, want it to name the option that resolves it", err)
	}
}

// TestSkipHostnameVerifyStillChecksKeyUsage: the manual verification has to
// keep the server-auth requirement, or a client certificate from the same CA
// would be accepted as a broker.
func TestSkipHostnameVerifyStillChecksKeyUsage(t *testing.T) {
	dir := t.TempDir()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(2),
		Subject:               pkix.Name{CommonName: "client only"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageClientAuth},
		BasicConstraintsValid: true,
	}
	der, err := x509.CreateCertificate(rand.Reader, &tmpl, &tmpl, &key.PublicKey, key)
	if err != nil {
		t.Fatal(err)
	}
	certPEM := pem.EncodeToMemory(&pem.Block{Type: "CERTIFICATE", Bytes: der})
	keyDER, err := x509.MarshalECPrivateKey(key)
	if err != nil {
		t.Fatal(err)
	}
	cert, err := tls.X509KeyPair(certPEM, pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER}))
	if err != nil {
		t.Fatal(err)
	}
	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}})
	tb := serveTestBroker(ln)
	t.Cleanup(func() { tb.Close() })

	err = New().Connect(config.Mqtt{
		Server: tb.Addr(), CaPath: caPath, User: "hal", SkipHostnameVerify: true,
	})
	if err == nil {
		t.Fatal("Connect() = nil: a certificate valid only for client authentication was accepted as a broker")
	}
}
