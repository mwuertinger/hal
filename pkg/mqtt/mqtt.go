package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io/ioutil"
	"log"

	"github.com/mwuertinger/hau/pkg/config"
	"github.com/pkg/errors"

	gmq_mqtt "github.com/yosssi/gmq/mqtt"
	gmq "github.com/yosssi/gmq/mqtt/client"
)

type Service interface {
	Connect(mqttConfig config.Mqtt) error
	Disconnect() error
	SendCommand(device string, status bool) error
}

type srv struct {
	client *gmq.Client
}

func New() Service {
	return &srv{}
}

func (s *srv) Connect(mqttConfig config.Mqtt) error {
	if s.client != nil {
		return errors.New("already connected")
	}

	// Load CA cert
	caCert, err := ioutil.ReadFile(mqttConfig.CaPath)
	if err != nil {
		return fmt.Errorf("unable to load 'MqttCaPath': %v", err)
	}
	caCertPool := x509.NewCertPool()
	caCertPool.AppendCertsFromPEM(caCert)

	// Create an MQTT Client.
	s.client = gmq.New(&gmq.Options{
		// Define the processing of the error handler.
		ErrorHandler: func(err error) {
			log.Printf("MQTT error: %v", err)
		},
	})

	// Connect to the MQTT Server.
	err = s.client.Connect(&gmq.ConnectOptions{
		Network: "tcp",
		Address: mqttConfig.Server,
		TLSConfig: &tls.Config{
			RootCAs:            caCertPool,
			InsecureSkipVerify: true, // FIXME
		},
		ClientID: []byte(mqttConfig.User),
		UserName: []byte(mqttConfig.User),
		Password: []byte(mqttConfig.Password),
	})
	if err != nil {
		return fmt.Errorf("connect failed: %v", err)
	}

	err = s.client.Subscribe(&gmq.SubscribeOptions{SubReqs: []*gmq.SubReq{{
		TopicFilter: []byte("+/+/+"),
		Handler: func(topicName, message []byte) {
			log.Printf("%s: %s", string(topicName), string(message))
		},
	}}})
	if err != nil {
		return fmt.Errorf("subscribe failed: %v", err)
	}

	return nil
}

func (s *srv) Disconnect() error {
	return s.client.Disconnect()
}

func (s *srv) SendCommand(device string, status bool) error {
	statusStr := ""
	if status {
		statusStr = "1"
	} else {
		statusStr = "0"
	}

	return s.client.Publish(&gmq.PublishOptions{
		QoS:       gmq_mqtt.QoS2,
		TopicName: []byte(fmt.Sprintf("cmnd/%s/POWER", device)),
		Message:   []byte(statusStr),
	})
}
