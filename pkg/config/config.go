package config

import (
	"fmt"
	"net"
	"os"

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
}

type Http struct {
	ListenAddress string `yaml:"listen-address"`
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
	err = validate(config)
	if err != nil {
		return nil, err
	}
	return config, nil
}

func unmarshal(config []byte) (*Config, error) {
	var c Config
	err := yaml.Unmarshal(config, &c)
	if err != nil {
		return nil, err
	}

	return &c, nil
}

// validate reports the first problem that would otherwise only surface once the
// broker connection or the HTTP listener is already being set up, by which point
// the failure is a log.Fatalf far away from the offending line of config.
func validate(config *Config) error {
	if len(config.Mqtt.Server) < 1 {
		return fmt.Errorf("mqtt.server must be set")
	}
	if _, _, err := net.SplitHostPort(config.Mqtt.Server); err != nil {
		return fmt.Errorf("mqtt.server %q is not host:port: %v", config.Mqtt.Server, err)
	}
	if len(config.Mqtt.CaPath) < 1 {
		return fmt.Errorf("mqtt.ca-path must be set")
	}

	if len(config.Http.ListenAddress) < 1 {
		return fmt.Errorf("http.listen-address must be set")
	}
	if _, _, err := net.SplitHostPort(config.Http.ListenAddress); err != nil {
		return fmt.Errorf("http.listen-address %q is not host:port: %v", config.Http.ListenAddress, err)
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
