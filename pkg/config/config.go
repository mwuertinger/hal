package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"strconv"
	"strings"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Mqtt    Mqtt
	Http    Http
	Devices []Device `yaml:"devices"`
}

type Mqtt struct {
	Server   string
	CaPath   string `yaml:"ca-path"`
	User     string
	Password string

	// SkipHostnameVerify keeps the certificate chain check against CaPath but
	// drops the check that the certificate names the host being connected to.
	// It exists for a broker whose certificate predates the requirement: Go has
	// ignored the Common Name field since 1.15, so a certificate with no
	// subjectAltName cannot be verified against any name at all.
	//
	// It is a real reduction: with it, any certificate signed by that CA is
	// accepted for this broker, not just the broker's own. Reissuing the
	// certificate with a subjectAltName is the fix; this is the stopgap.
	SkipHostnameVerify bool `yaml:"skip-hostname-verify"`
}

type Http struct {
	ListenAddress string `yaml:"listen-address"`

	// AllowedHosts names the extra Host header values the frontend answers to.
	// It is only needed for a domain name the local network does not own: IP
	// addresses, single-label names and the LAN suffixes are always accepted.
	// See hostCheck and lanSuffixes in pkg/frontend, which are authoritative.
	AllowedHosts []string `yaml:"allowed-hosts"`
}

type Device struct {
	ID       string
	Name     string
	Type     DeviceType
	Location string
}

// DeviceType selects the driver a device is handled by. These are the only values
// device.RegisterDevices accepts; keep deviceTypes below in sync when adding one.
type DeviceType string

const (
	DeviceTypeSonoffMqttSwitch DeviceType = "sonoff-mqtt-switch"
)

var deviceTypes = []DeviceType{
	DeviceTypeSonoffMqttSwitch,
}

func (t DeviceType) valid() bool {
	for _, known := range deviceTypes {
		if t == known {
			return true
		}
	}
	return false
}

func Load(path string) (*Config, error) {
	buf, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	config, err := unmarshal(buf)
	if err != nil {
		return nil, err
	}
	normalise(config)
	err = validate(config)
	if err != nil {
		return nil, err
	}
	return config, nil
}

func unmarshal(config []byte) (*Config, error) {
	var c Config

	// KnownFields, so that a key HAL does not understand is an error rather
	// than a silent omission. Writing "device:" for "devices:" used to start
	// cleanly, log "Devices registered", and serve an empty page.
	dec := yaml.NewDecoder(bytes.NewReader(config))
	dec.KnownFields(true)
	if err := dec.Decode(&c); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}

	return &c, nil
}

// defaultLocation groups devices whose config gives no location. The frontend
// keys rooms on this string, so leaving it empty rendered a room with a blank
// heading rather than anything a reader could act on.
const defaultLocation = "Other"

// normalise cleans up input that is meant rather than mistyped: surrounding
// whitespace, which YAML keeps inside a quoted scalar and which then reaches
// the resolver as part of a hostname, and a missing location.
func normalise(config *Config) {
	config.Mqtt.Server = strings.TrimSpace(config.Mqtt.Server)
	config.Mqtt.CaPath = strings.TrimSpace(config.Mqtt.CaPath)
	config.Http.ListenAddress = strings.TrimSpace(config.Http.ListenAddress)

	// Untrimmed, a leading space here means the name never matches and every
	// request for it is refused with 421, with nothing in the log to say why.
	hosts := config.Http.AllowedHosts[:0]
	for _, host := range config.Http.AllowedHosts {
		if host = strings.TrimSpace(host); host != "" {
			hosts = append(hosts, host)
		}
	}
	config.Http.AllowedHosts = hosts

	for i := range config.Devices {
		d := &config.Devices[i]
		d.ID = strings.TrimSpace(d.ID)
		d.Name = strings.TrimSpace(d.Name)
		d.Location = strings.TrimSpace(d.Location)
		if d.Location == "" {
			d.Location = defaultLocation
		}
	}
}

// checkHostPort rejects what SplitHostPort accepts but the network layer does
// not. SplitHostPort only splits: "broker:" and ":0" both pass it, and then
// fail much later as "missing port in address" and as a bind on a random port
// that SocketBindAllow= in hal.service denies.
func checkHostPort(address string, hostRequired bool) error {
	host, port, err := net.SplitHostPort(address)
	if err != nil {
		return fmt.Errorf("is not host:port: %v", err)
	}
	if hostRequired && host == "" {
		return errors.New("host must be set")
	}
	n, err := strconv.Atoi(port)
	if err != nil {
		// A named port resolves through /etc/services, which the target need
		// not have; keep it to numbers so the config says what it means.
		return fmt.Errorf("port %q is not a number", port)
	}
	if n < 1 || n > 65535 {
		return fmt.Errorf("port %d is out of range 1-65535", n)
	}
	return nil
}

// validate reports the first problem that would otherwise only surface once the
// broker connection or the HTTP listener is already being set up, by which point
// the failure is a log.Fatalf far away from the offending line of config.
func validate(config *Config) error {
	if len(config.Mqtt.Server) < 1 {
		return fmt.Errorf("mqtt.server must be set")
	}
	if err := checkHostPort(config.Mqtt.Server, true); err != nil {
		return fmt.Errorf("mqtt.server %q: %v", config.Mqtt.Server, err)
	}
	if len(config.Mqtt.CaPath) < 1 {
		return fmt.Errorf("mqtt.ca-path must be set")
	}

	if len(config.Http.ListenAddress) < 1 {
		return fmt.Errorf("http.listen-address must be set")
	}
	if err := checkHostPort(config.Http.ListenAddress, false); err != nil {
		return fmt.Errorf("http.listen-address %q: %v", config.Http.ListenAddress, err)
	}

	for i, host := range config.Http.AllowedHosts {
		// The port is stripped from the incoming Host before it is compared, so
		// an entry carrying one can never match - and the symptom is a
		// permanent 421 with nothing in the log to explain it. Easy to write,
		// given http.listen-address directly above it takes a port.
		if strings.ContainsAny(host, ":/") {
			return fmt.Errorf("http.allowed-hosts[%d] %q: a host name only, without a port or scheme", i, host)
		}
	}

	seen := make(map[string]int, len(config.Devices))
	for i, d := range config.Devices {
		if len(d.ID) < 1 {
			return fmt.Errorf("devices[%d]: id must be set", i)
		}
		if first, ok := seen[d.ID]; ok {
			return fmt.Errorf("devices[%d]: duplicate id %q, already used by devices[%d]", i, d.ID, first)
		}
		seen[d.ID] = i

		if len(d.Name) < 1 {
			return fmt.Errorf("devices[%d] (%s): name must be set", i, d.ID)
		}
		if !d.Type.valid() {
			return fmt.Errorf("devices[%d] (%s): invalid type %q, must be one of %v", i, d.ID, d.Type, deviceTypes)
		}
	}

	return nil
}
