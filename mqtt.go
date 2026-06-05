package main

import (
	"fmt"
	"github.com/eclipse/paho.mqtt.golang"
	log "github.com/sirupsen/logrus"
	"strings"
	"sync"
)

type MqttConnectedHandler func()

type Subscription struct {
	topic    string
	qos      byte
	callback MessageHandler
}

type Mqtt struct {
	client            mqtt.Client
	config            *MqttConfig
	connectedHandlers []MqttConnectedHandler
	subscriptions     []Subscription
	subsMutex         sync.Mutex
}

type MessageHandler func(payload []byte)

func ConnectMqtt(config *Config) (*Mqtt, error) {
	mqttConfig := config.Mqtt
	options := mqtt.NewClientOptions()

	options.AddBroker(mqttConfig.Host)
	options.SetClientID("cec2mqtt-" + mqttConfig.BaseTopic)

	if mqttConfig.Username != "" {
		options.SetUsername(mqttConfig.Username)
	}

	if mqttConfig.Password != "" {
		options.SetPassword(mqttConfig.Password)
	}

	if mqttConfig.StateTopic != "" {
		if mqttConfig.WillMessage != "" {
			options.SetWill(mqttConfig.StateTopic, mqttConfig.WillMessage, 0, true)
		}
	}

	inst := &Mqtt{
		config: &mqttConfig,
	}

	options.SetOnConnectHandler(func(client mqtt.Client) {
		log.Info("Connected to MQTT")
		if mqttConfig.StateTopic != "" && mqttConfig.BirthMessage != "" {
			client.Publish(mqttConfig.StateTopic, 0, true, mqttConfig.BirthMessage)
		}

		inst.subsMutex.Lock()
		for _, sub := range inst.subscriptions {
			inst.subscribe(sub.topic, sub.qos, sub.callback)
		}
		inst.subsMutex.Unlock()

		for _, handler := range inst.connectedHandlers {
			handler()
		}
	})

	client := mqtt.NewClient(options)
	inst.client = client

	connToken := client.Connect()

	connToken.Wait()

	if err := connToken.Error(); err != nil {
		return nil, err
	}

	return inst, nil
}

func (mqtt *Mqtt) BuildTopic(device *Device, suffix string) string {
	topic := strings.Builder{}
	fmt.Fprintf(&topic, "%s/%s/%s", mqtt.config.BaseTopic, device.Config.MqttTopic, suffix)
	return topic.String()
}

func (mqtt *Mqtt) BuildBridgeTopic(suffix string) string {
	topic := strings.Builder{}
	fmt.Fprintf(&topic, "%s/%s", mqtt.config.BaseTopic, suffix)
	return topic.String()
}

func (mqtt *Mqtt) Publish(topic string, qos byte, retained bool, payload interface{}) {
	mqtt.client.Publish(topic, qos, retained, payload)
	log.WithFields(log.Fields{
		"topic":    topic,
		"qos":      qos,
		"retained": retained,
		"payload":  payload,
	}).Trace("Published MQTT message")
}

func (m *Mqtt) Subscribe(topic string, qos byte, callback MessageHandler) {
	m.subsMutex.Lock()
	m.subscriptions = append(m.subscriptions, Subscription{topic, qos, callback})
	m.subsMutex.Unlock()

	m.subscribe(topic, qos, callback)
}

func (m *Mqtt) subscribe(topic string, qos byte, callback MessageHandler) {
	m.client.Subscribe(topic, qos, func(_ mqtt.Client, message mqtt.Message) {
		log.WithFields(log.Fields{
			"topic":   message.Topic(),
			"payload": message.Payload(),
		}).Trace("Handling incoming MQTT message")
		callback(message.Payload())
	})
	log.WithFields(log.Fields{
		"topic": topic,
		"qos":   qos,
	}).Trace("Subscribed to MQTT topic")
}

func (m *Mqtt) RegisterConnectedHandler(handler MqttConnectedHandler) {
	m.connectedHandlers = append(m.connectedHandlers, handler)
}
