package config

import (
	"reflect"
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
  type:     socket
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
				Type:     "socket",
				Name:     "Floor Lamp",
				Location: "Living Room",
			},
		},
	}

	if !reflect.DeepEqual(*config, expectedConfig) {
		t.Fatalf("not the expected config: %v", *config)
	}
}
