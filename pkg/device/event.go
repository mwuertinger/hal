package device

import "time"

type Event struct {
	Timestamp time.Time
	DeviceId  string
	Payload   EventPayload
}

type EventPayload interface {
}

type EventPayloadSwitch struct {
	State bool
}
