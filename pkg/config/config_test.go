package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

const configYaml = `
mqtt:
  ca-path:        ca.crt
  server:         mqtt.example.com:1883
  user:           foo
  password:       bar
http:
  listen-address: fizz:1234
devices:
- id:       socket01
  type:     sonoff-mqtt-switch
  name:     Floor Lamp
  location: Living Room
`

func TestUnmarshal(t *testing.T) {
	config, err := unmarshal([]byte(configYaml))
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	expectedConfig := Config{
		Mqtt: Mqtt{
			CaPath:   "ca.crt",
			Server:   "mqtt.example.com:1883",
			User:     "foo",
			Password: "bar",
		},
		Http: Http{
			ListenAddress: "fizz:1234",
		},
		Devices: []Device{
			{
				ID:       "socket01",
				Type:     DeviceTypeSonoffMqttSwitch,
				Name:     "Floor Lamp",
				Location: "Living Room",
			},
		},
	}

	if !reflect.DeepEqual(*config, expectedConfig) {
		// Printed field by field rather than as a whole struct, which would put
		// the MQTT password in the test output.
		t.Fatalf("not the expected config: mqtt=%s/%s http=%+v devices=%+v",
			config.Mqtt.Server, config.Mqtt.CaPath, config.Http, config.Devices)
	}
}

// valid returns a config that passes validate(), so that each test case can break
// exactly one thing and nothing else.
func valid() *Config {
	return &Config{
		Mqtt: Mqtt{
			Server: "mqtt.example.com:8883",
			CaPath: "/etc/hal/ca.crt",
		},
		Http: Http{
			ListenAddress: ":8080",
		},
		Devices: []Device{
			{ID: "socket01", Name: "Floor Lamp", Type: DeviceTypeSonoffMqttSwitch, Location: "Living Room"},
			{ID: "socket02", Name: "Desk Lamp", Type: DeviceTypeSonoffMqttSwitch, Location: "Office"},
		},
	}
}

func TestValidate(t *testing.T) {
	tests := []struct {
		name string
		// mutate breaks the otherwise valid config in exactly one way.
		mutate func(*Config)
		// wantErr is a substring of the expected error; empty means no error.
		wantErr string
	}{
		{
			name:   "valid",
			mutate: func(*Config) {},
		},
		{
			name:   "no devices is allowed",
			mutate: func(c *Config) { c.Devices = nil },
		},
		{
			name:    "missing mqtt server",
			mutate:  func(c *Config) { c.Mqtt.Server = "" },
			wantErr: "mqtt.server must be set",
		},
		{
			name:    "mqtt server without port",
			mutate:  func(c *Config) { c.Mqtt.Server = "mqtt.example.com" },
			wantErr: "is not host:port",
		},
		{
			name:    "missing ca path",
			mutate:  func(c *Config) { c.Mqtt.CaPath = "" },
			wantErr: "mqtt.ca-path must be set",
		},
		{
			name:    "missing listen address",
			mutate:  func(c *Config) { c.Http.ListenAddress = "" },
			wantErr: "http.listen-address must be set",
		},
		{
			name:    "listen address without port",
			mutate:  func(c *Config) { c.Http.ListenAddress = "8080" },
			wantErr: "is not host:port",
		},
		{
			name:    "missing device id",
			mutate:  func(c *Config) { c.Devices[1].ID = "" },
			wantErr: "devices[1]: id must be set",
		},
		{
			name:    "duplicate device id",
			mutate:  func(c *Config) { c.Devices[1].ID = "socket01" },
			wantErr: `devices[1]: duplicate id "socket01", already used by devices[0]`,
		},
		{
			name:    "missing device name",
			mutate:  func(c *Config) { c.Devices[1].Name = "" },
			wantErr: "devices[1] (socket02): name must be set",
		},
		{
			name:    "missing device type",
			mutate:  func(c *Config) { c.Devices[1].Type = "" },
			wantErr: `devices[1] (socket02): invalid type ""`,
		},
		{
			// "socket" was declared as a DeviceType constant but never accepted by
			// device.addDevice, so it used to fail at registration instead.
			name:    "device type that is not a driver",
			mutate:  func(c *Config) { c.Devices[1].Type = "socket" },
			wantErr: `devices[1] (socket02): invalid type "socket", must be one of [sonoff-mqtt-switch]`,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			config := valid()
			tt.mutate(config)

			err := validate(config)

			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("validate() = %v, want nil", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("validate() = nil, want error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Fatalf("validate() = %q, want error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestUnknownKeysAreRejected covers the silent-omission bug: "device:" for
// "devices:" used to unmarshal into nothing, pass validation, and start HAL
// with a green systemctl status and an empty page.
func TestUnknownKeysAreRejected(t *testing.T) {
	tests := []struct {
		name    string
		yaml    string
		wantErr string
	}{
		{
			name: "a mistyped top-level key",
			yaml: strings.ReplaceAll(configYaml, "devices:", "device:"),
			// yaml.v3 spells this "field <name> not found in type ...".
			wantErr: "device not found",
		},
		{
			name:    "a mistyped nested key",
			yaml:    strings.ReplaceAll(configYaml, "ca-path:", "capath:"),
			wantErr: "capath not found",
		},
		{
			name:    "an unknown section",
			yaml:    configYaml + "\nlogging:\n  level: debug\n",
			wantErr: "logging not found",
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := unmarshal([]byte(tt.yaml))
			if err == nil {
				t.Fatalf("unmarshal() = nil, want an error containing %q", tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("unmarshal() = %q, want an error containing %q", err, tt.wantErr)
			}
		})
	}
}

// TestExampleConfigLoads pins hal.yaml.example against the struct. It is what
// every deployment is copied from, and nothing else parses it.
func TestExampleConfigLoads(t *testing.T) {
	c, err := Load("../../hal.yaml.example")
	if err != nil {
		t.Fatalf("hal.yaml.example does not load: %v", err)
	}
	if len(c.Devices) == 0 {
		t.Error("the example config documents no devices")
	}
}

func TestLoadErrors(t *testing.T) {
	if _, err := Load(filepath.Join(t.TempDir(), "nope.yaml")); err == nil {
		t.Error("Load() of a missing file = nil, want an error")
	}

	broken := filepath.Join(t.TempDir(), "broken.yaml")
	if err := os.WriteFile(broken, []byte("mqtt: ["), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(broken); err == nil {
		t.Error("Load() of malformed YAML = nil, want an error")
	}

	// Load must validate, not just unmarshal.
	incomplete := filepath.Join(t.TempDir(), "incomplete.yaml")
	if err := os.WriteFile(incomplete, []byte("http:\n  listen-address: :8080\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := Load(incomplete); err == nil {
		t.Error("Load() of a config with no mqtt.server = nil, want an error")
	}
}

// TestNormalise covers the two inputs that are meant rather than mistyped:
// surrounding whitespace, which YAML keeps inside a quoted scalar and which
// then reaches the resolver as part of the hostname, and a missing location,
// which used to render a room with a blank heading.
func TestNormalise(t *testing.T) {
	c := &Config{
		Mqtt: Mqtt{Server: "  mqtt.example.com:8883  ", CaPath: " /etc/hal/ca.crt "},
		Http: Http{ListenAddress: " :8080 "},
		Devices: []Device{
			{ID: " socket01 ", Name: " Lamp ", Type: DeviceTypeSonoffMqttSwitch},
			{ID: "socket02", Name: "Lamp 2", Type: DeviceTypeSonoffMqttSwitch, Location: "Office"},
		},
	}

	normalise(c)

	if c.Mqtt.Server != "mqtt.example.com:8883" {
		t.Errorf("Mqtt.Server = %q, still padded", c.Mqtt.Server)
	}
	if c.Mqtt.CaPath != "/etc/hal/ca.crt" {
		t.Errorf("Mqtt.CaPath = %q, still padded", c.Mqtt.CaPath)
	}
	if c.Http.ListenAddress != ":8080" {
		t.Errorf("Http.ListenAddress = %q, still padded", c.Http.ListenAddress)
	}
	if c.Devices[0].ID != "socket01" || c.Devices[0].Name != "Lamp" {
		t.Errorf("device[0] = %+v, still padded", c.Devices[0])
	}
	if c.Devices[0].Location != defaultLocation {
		t.Errorf("device[0].Location = %q, want %q", c.Devices[0].Location, defaultLocation)
	}
	if c.Devices[1].Location != "Office" {
		t.Errorf("device[1].Location = %q, want it left alone", c.Devices[1].Location)
	}

	if err := validate(c); err != nil {
		t.Errorf("validate() after normalise() = %v", err)
	}
}

// TestCheckHostPort covers what net.SplitHostPort accepts but the network layer
// does not: it only splits, so "broker:" and ":0" both used to pass validation
// and then fail much later, the second as a bind that SocketBindAllow= denies.
func TestCheckHostPort(t *testing.T) {
	tests := []struct {
		address      string
		hostRequired bool
		wantErr      string
	}{
		{address: "mqtt.example.com:8883", hostRequired: true},
		{address: ":8080"},
		{address: "127.0.0.1:8080"},
		{address: "[fd00::1]:8883", hostRequired: true},
		{address: "mqtt.example.com", hostRequired: true, wantErr: "is not host:port"},
		{address: "broker:", hostRequired: true, wantErr: "not a number"},
		{address: ":8883", hostRequired: true, wantErr: "host must be set"},
		{address: ":0", wantErr: "out of range"},
		{address: ":70000", wantErr: "out of range"},
		{address: "broker:secure-mqtt", hostRequired: true, wantErr: "not a number"},
		{address: "fd00::1:8883", hostRequired: true, wantErr: "too many colons"},
	}

	for _, tt := range tests {
		t.Run(tt.address, func(t *testing.T) {
			err := checkHostPort(tt.address, tt.hostRequired)
			if tt.wantErr == "" {
				if err != nil {
					t.Fatalf("checkHostPort(%q) = %v, want nil", tt.address, err)
				}
				return
			}
			if err == nil {
				t.Fatalf("checkHostPort(%q) = nil, want an error containing %q", tt.address, tt.wantErr)
			}
			if !strings.Contains(err.Error(), tt.wantErr) {
				t.Errorf("checkHostPort(%q) = %q, want an error containing %q", tt.address, err, tt.wantErr)
			}
		})
	}
}

// TestLoadNormalises pins that Load runs normalise, not just validate: a
// device with no location has to arrive at the frontend already grouped, and
// the normalisation is invisible from validate()'s result.
func TestLoadNormalises(t *testing.T) {
	path := filepath.Join(t.TempDir(), "hal.yaml")
	yaml := `
mqtt:
  server:         "  mqtt.example.com:8883  "
  ca-path:        /etc/hal/ca.crt
http:
  listen-address: :8080
devices:
- id:   socket01
  type: sonoff-mqtt-switch
  name: Floor Lamp
`
	if err := os.WriteFile(path, []byte(yaml), 0o600); err != nil {
		t.Fatal(err)
	}

	c, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if c.Mqtt.Server != "mqtt.example.com:8883" {
		t.Errorf("Mqtt.Server = %q, want it trimmed", c.Mqtt.Server)
	}
	if c.Devices[0].Location != defaultLocation {
		t.Errorf("Devices[0].Location = %q, want %q", c.Devices[0].Location, defaultLocation)
	}
}

// TestNormaliseAllowedHosts: an untrimmed entry matches nothing, so every
// request for that name is refused with 421 and no log line explains it.
func TestNormaliseAllowedHosts(t *testing.T) {
	c := valid()
	c.Http.AllowedHosts = []string{" hal.example.com", "", "  ", "hal.example.org\t"}

	normalise(c)

	want := []string{"hal.example.com", "hal.example.org"}
	if !reflect.DeepEqual(c.Http.AllowedHosts, want) {
		t.Errorf("AllowedHosts = %q, want %q", c.Http.AllowedHosts, want)
	}
}
