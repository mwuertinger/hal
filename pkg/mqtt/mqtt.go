package mqtt

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"io"
	"io/ioutil"
	"log"
	"sync"
	"time"

	"github.com/mwuertinger/hal/pkg/config"
	"github.com/pkg/errors"

	gmq_mqtt "github.com/yosssi/gmq/mqtt"
	gmq "github.com/yosssi/gmq/mqtt/client"
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
	Subscribe(topic string) (<-chan Notification, error)
}

type broker struct {
	client        *gmq.Client
	subscribers   []chan Notification
	subscribersMu sync.Mutex
	shutdown      chan interface{} // closed on shutdown
}

func New() Broker {
	return &broker{
		shutdown: make(chan interface{}),
	}
}

func (s *broker) Connect(mqttConfig config.Mqtt) error {
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

	connectOpts := gmq.ConnectOptions{
		Network: "tcp",
		Address: mqttConfig.Server,
		TLSConfig: &tls.Config{
			RootCAs:            caCertPool,
			InsecureSkipVerify: true, // FIXME
		},
		ClientID: []byte(mqttConfig.User),
		UserName: []byte(mqttConfig.User),
		Password: []byte(mqttConfig.Password),
	}

	// Create an MQTT Client.
	s.client = gmq.New(&gmq.Options{
		// Define the processing of the error handler.
		ErrorHandler: func(err error) {
			log.Printf("MQTT error: %v", err)
			if err == io.EOF {
				log.Print("Trying to reconnect...")
				err = s.client.Connect(&connectOpts)
				if err != nil {
					log.Fatalf("reconnect failed: %v", err)
				}
			}
		},
	})

	// Connect to the MQTT Server.
	err = s.client.Connect(&connectOpts)
	if err != nil {
		return fmt.Errorf("connect failed: %v", err)
	}

	log.Println("MQTT connection established")

	return nil
}

func (s *broker) Shutdown() {
	if err := s.client.Disconnect(); err != nil {
		log.Printf("mqtt.Disconnect(): %v", err)
	}

	s.subscribersMu.Lock()
	for _, subscriber := range s.subscribers {
		close(subscriber)
	}
	s.subscribers = nil
	s.subscribersMu.Unlock()

	log.Printf("broker: shutdown complete")
}

func (s *broker) Publish(topic string, msg string) error {
	return s.client.Publish(&gmq.PublishOptions{
		QoS:       gmq_mqtt.QoS2,
		TopicName: []byte(topic),
		Message:   []byte(msg),
	})
}

func (s *broker) Subscribe(topic string) (<-chan Notification, error) {
	c := make(chan Notification)

	s.subscribersMu.Lock()
	defer s.subscribersMu.Unlock()

	err := s.client.Subscribe(&gmq.SubscribeOptions{SubReqs: []*gmq.SubReq{{
		TopicFilter: []byte(topic),
		Handler: func(topicName, message []byte) {
			c <- Notification{time.Now(), string(topicName), string(message)}
		},
	}}})
	if err != nil {
		return nil, err
	}

	s.subscribers = append(s.subscribers, c)
	return c, nil
}

type fakeBroker struct {
}

func (s *fakeBroker) Subscribe(topic string) (<-chan Notification, error) {
	return make(chan Notification), nil
}

func (s *fakeBroker) Connect(mqttConfig config.Mqtt) error {
	return nil
}

func (s *fakeBroker) Shutdown() {
}

func (s *fakeBroker) Publish(topic string, msg string) error {
	return nil
}

func NewFake() Broker {
	return &fakeBroker{}
}
