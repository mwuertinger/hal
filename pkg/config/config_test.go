package config

import (
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
		t.Fatalf("unexpected error: ")
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
		t.Fatalf("not the expected config: %v", *config)
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
