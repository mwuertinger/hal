package config

import (
	"gopkg.in/yaml.v2"
	"io/ioutil"
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
	Type     string
	Location string
}

type DeviceType string

const (
	DeviceType_Socket DeviceType = "socket"
)

func Load(path string) (*Config, error) {
	buf, err := ioutil.ReadFile(path)
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

func validate(config *Config) error {
	return nil
}
