package mavlink

import (
	"fmt"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

// IsHeartbeat reports whether m is a HEARTBEAT.
func IsHeartbeat(m message.Message) bool {
	_, ok := m.(*ardupilotmega.MessageHeartbeat)
	return ok
}

// copterModes are ArduCopter's flight mode numbers (HEARTBEAT.custom_mode),
// from ArduPilot's Mode::Number.
var copterModes = map[uint32]string{
	0: "STABILIZE", 1: "ACRO", 2: "ALT_HOLD", 3: "AUTO", 4: "GUIDED", 5: "LOITER",
	6: "RTL", 7: "CIRCLE", 9: "LAND", 11: "DRIFT", 13: "SPORT", 14: "FLIP",
	15: "AUTOTUNE", 16: "POSHOLD", 17: "BRAKE", 18: "THROW", 19: "AVOID_ADSB",
	20: "GUIDED_NOGPS", 21: "SMART_RTL", 22: "FLOWHOLD", 23: "FOLLOW", 24: "ZIGZAG",
	25: "SYSTEMID", 26: "AUTOROTATE", 27: "AUTO_RTL", 28: "TURTLE",
}

// ModeName returns the flight mode in an ArduPilot copter HEARTBEAT, such as
// "GUIDED", or "MODE n" for other autopilots and unknown numbers.
func ModeName(hb *ardupilotmega.MessageHeartbeat) string {
	if hb.Autopilot == ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA {
		if name, ok := copterModes[hb.CustomMode]; ok {
			return name
		}
	}
	return fmt.Sprintf("MODE %d", hb.CustomMode)
}

// IsArmed reports whether a HEARTBEAT has the armed flag set.
func IsArmed(hb *ardupilotmega.MessageHeartbeat) bool {
	return hb.BaseMode&ardupilotmega.MAV_MODE_FLAG_SAFETY_ARMED != 0
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
