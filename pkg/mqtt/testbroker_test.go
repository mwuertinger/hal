package mqtt

// testBroker is a minimal MQTT 3.1.1 server: CONNECT/CONNACK, SUBSCRIBE/SUBACK,
// PUBLISH in both directions, PINGREQ/PINGRESP and DISCONNECT, and no more.
//
// It exists because everything interesting in this package is what the client
// does on the wire - a publish issued while it is reconnecting, a subscription
// the broker refuses, whether subscriptions come back after a hangup - and none
// of that is reachable without a broker to talk to. Writing ~250 lines here
// beats adding a dependency to test a dependency.

import (
	"crypto/ecdsa"
	"crypto/elliptic"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"crypto/x509/pkix"
	"encoding/binary"
	"encoding/pem"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

type testBroker struct {
	ln net.Listener

	mu          sync.Mutex
	subscribers map[string][]net.Conn
	published   []Published
	connects    int
	refuseSubs  bool
	refuseConns bool
	// ConnackCode, when non-zero, refuses every connection with that code.
	ConnackCode byte
}

// Serve runs the broker on an existing listener, so a test can wrap it in TLS.
func serveTestBroker(ln net.Listener) *testBroker {
	b := &testBroker{ln: ln, subscribers: make(map[string][]net.Conn)}
	go b.accept()
	return b
}

// startTestBroker runs the broker behind TLS on loopback and returns it with
// the path of a CA file that verifies it, so the client's real TLS path is
// exercised rather than bypassed.
func startTestBroker(t *testing.T) (*testBroker, string) {
	t.Helper()

	dir := t.TempDir()
	cert, caPath := serverCert(t, dir)

	raw, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ln := tls.NewListener(raw, &tls.Config{Certificates: []tls.Certificate{cert}})

	b := serveTestBroker(ln)
	t.Cleanup(func() { b.Close() })
	return b, caPath
}

// serverCert returns a self-signed certificate valid for 127.0.0.1, and the
// path of a CA file containing it.
func serverCert(t *testing.T, dir string) (tls.Certificate, string) {
	t.Helper()

	key, err := ecdsa.GenerateKey(elliptic.P256(), rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	tmpl := x509.Certificate{
		SerialNumber:          big.NewInt(1),
		Subject:               pkix.Name{CommonName: "hal test broker"},
		NotBefore:             time.Now().Add(-time.Hour),
		NotAfter:              time.Now().Add(time.Hour),
		IsCA:                  true,
		KeyUsage:              x509.KeyUsageCertSign | x509.KeyUsageDigitalSignature,
		ExtKeyUsage:           []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
		IPAddresses:           []net.IP{net.ParseIP("127.0.0.1")},
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
	keyPEM := pem.EncodeToMemory(&pem.Block{Type: "EC PRIVATE KEY", Bytes: keyDER})

	cert, err := tls.X509KeyPair(certPEM, keyPEM)
	if err != nil {
		t.Fatal(err)
	}

	caPath := filepath.Join(dir, "ca.pem")
	if err := os.WriteFile(caPath, certPEM, 0o600); err != nil {
		t.Fatal(err)
	}
	return cert, caPath
}

func (b *testBroker) Addr() string { return b.ln.Addr().String() }
func (b *testBroker) Close() error { return b.ln.Close() }

func (b *testBroker) Connects() int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.connects
}

// RefuseSubscriptions makes every SUBACK carry 0x80, as a broker whose ACL does
// not grant the topic does.
func (b *testBroker) RefuseSubscriptions(refuse bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuseSubs = refuse
}

// RefuseConnections hangs up on every CONNECT, so a client is held in its
// reconnect loop for as long as it is set.
func (b *testBroker) RefuseConnections(refuse bool) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.refuseConns = refuse
}

// Published returns what clients published to the broker, oldest first.
func (b *testBroker) Published() []Published {
	b.mu.Lock()
	defer b.mu.Unlock()
	return append([]Published(nil), b.published...)
}

func (b *testBroker) Topics() []string {
	b.mu.Lock()
	defer b.mu.Unlock()
	var out []string
	for t := range b.subscribers {
		out = append(out, t)
	}
	return out
}

func (b *testBroker) accept() {
	for {
		conn, err := b.ln.Accept()
		if err != nil {
			return
		}
		go b.serve(conn)
	}
}

// Publish sends a PUBLISH (QoS 0) to every subscriber of topic.
func (b *testBroker) Publish(topic, payload string) int {
	b.mu.Lock()
	conns := append([]net.Conn(nil), b.subscribers[topic]...)
	b.mu.Unlock()

	var frame []byte
	frame = append(frame, 0x30)
	var body []byte
	body = appendString(body, topic)
	body = append(body, payload...)
	frame = appendRemainingLength(frame, len(body))
	frame = append(frame, body...)

	sent := 0
	for _, c := range conns {
		if _, err := c.Write(frame); err == nil {
			sent++
		}
	}
	return sent
}

// DropAll hangs up on every connected client without a DISCONNECT, which is
// what a broker restart or a lost route looks like to the client.
func (b *testBroker) DropAll() {
	b.mu.Lock()
	seen := make(map[net.Conn]bool)
	for _, conns := range b.subscribers {
		for _, c := range conns {
			seen[c] = true
		}
	}
	b.subscribers = make(map[string][]net.Conn)
	b.mu.Unlock()

	for c := range seen {
		c.Close()
	}
}

func (b *testBroker) serve(conn net.Conn) {
	defer conn.Close()

	for {
		header := make([]byte, 1)
		if _, err := io.ReadFull(conn, header); err != nil {
			b.forget(conn)
			return
		}
		length, err := readRemainingLength(conn)
		if err != nil {
			b.forget(conn)
			return
		}
		body := make([]byte, length)
		if _, err := io.ReadFull(conn, body); err != nil {
			b.forget(conn)
			return
		}

		switch header[0] >> 4 {
		case 1: // CONNECT
			b.mu.Lock()
			b.connects++
			code := b.ConnackCode
			refuse := b.refuseConns
			b.mu.Unlock()
			if refuse {
				return
			}
			if _, err := conn.Write([]byte{0x20, 0x02, 0x00, code}); err != nil || code != 0 {
				return
			}
		case 8: // SUBSCRIBE
			packetID := binary.BigEndian.Uint16(body[:2])
			rest := body[2:]
			var codes []byte
			for len(rest) > 0 {
				n := int(binary.BigEndian.Uint16(rest[:2]))
				topic := string(rest[2 : 2+n])
				qos := rest[2+n]
				rest = rest[3+n:]

				b.mu.Lock()
				refuse := b.refuseSubs
				if !refuse {
					b.subscribers[topic] = append(b.subscribers[topic], conn)
				}
				b.mu.Unlock()

				if refuse {
					codes = append(codes, 0x80)
				} else {
					codes = append(codes, qos)
				}
			}
			ack := []byte{0x90}
			ack = appendRemainingLength(ack, 2+len(codes))
			ack = binary.BigEndian.AppendUint16(ack, packetID)
			ack = append(ack, codes...)
			if _, err := conn.Write(ack); err != nil {
				return
			}
		case 3: // PUBLISH from the client
			n := int(binary.BigEndian.Uint16(body[:2]))
			topic := string(body[2 : 2+n])
			payload := body[2+n:]
			if qos := (header[0] >> 1) & 0x03; qos == 1 {
				packetID := binary.BigEndian.Uint16(body[2+n : 4+n])
				payload = body[4+n:]
				ack := []byte{0x40, 0x02}
				ack = binary.BigEndian.AppendUint16(ack, packetID)
				if _, err := conn.Write(ack); err != nil {
					return
				}
			}
			b.mu.Lock()
			b.published = append(b.published, Published{Topic: topic, Msg: string(payload)})
			b.mu.Unlock()
		case 12: // PINGREQ
			if _, err := conn.Write([]byte{0xD0, 0x00}); err != nil {
				return
			}
		case 14: // DISCONNECT
			b.forget(conn)
			return
		}
	}
}

func (b *testBroker) forget(conn net.Conn) {
	b.mu.Lock()
	defer b.mu.Unlock()
	for topic, conns := range b.subscribers {
		remaining := conns[:0]
		for _, c := range conns {
			if c != conn {
				remaining = append(remaining, c)
			}
		}
		if len(remaining) == 0 {
			delete(b.subscribers, topic)
		} else {
			b.subscribers[topic] = remaining
		}
	}
}

func appendString(b []byte, s string) []byte {
	b = binary.BigEndian.AppendUint16(b, uint16(len(s)))
	return append(b, s...)
}

func appendRemainingLength(b []byte, n int) []byte {
	for {
		digit := byte(n % 128)
		n /= 128
		if n > 0 {
			digit |= 0x80
		}
		b = append(b, digit)
		if n == 0 {
			return b
		}
	}
}

func readRemainingLength(r io.Reader) (int, error) {
	var value, multiplier int
	buf := make([]byte, 1)
	for i := 0; i < 4; i++ {
		if _, err := io.ReadFull(r, buf); err != nil {
			return 0, err
		}
		value += int(buf[0]&127) << multiplier
		if buf[0]&128 == 0 {
			return value, nil
		}
		multiplier += 7
	}
	return 0, fmt.Errorf("malformed remaining length")
}
