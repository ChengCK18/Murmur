package mavlink

import (
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

// IsHeartbeat reports whether m is a HEARTBEAT.
func IsHeartbeat(m message.Message) bool {
	_, ok := m.(*ardupilotmega.MessageHeartbeat)
	return ok
}

// TimeBootMs returns the message's time_boot_ms (milliseconds since the
// sender booted) for the message types ArduPilot streams with one.
func TimeBootMs(m message.Message) (uint32, bool) {
	switch m := m.(type) {
	case *ardupilotmega.MessageAttitude:
		return m.TimeBootMs, true
	case *ardupilotmega.MessageGlobalPositionInt:
		return m.TimeBootMs, true
	case *ardupilotmega.MessageLocalPositionNed:
		return m.TimeBootMs, true
	case *ardupilotmega.MessageSystemTime:
		return m.TimeBootMs, true
	case *ardupilotmega.MessagePositionTargetGlobalInt:
		return m.TimeBootMs, true
	}
	return 0, false
}
