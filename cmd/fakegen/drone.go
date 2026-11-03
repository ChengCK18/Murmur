package main

import (
	"math"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

// tick is the base rate: every rate below is a whole number of ticks.
const tick = 100 * time.Millisecond

// Home and layout: drones fly circles on a grid around ArduPilot's SITL home.
const (
	homeLat, homeLng = -35.363262, 149.165237
	homeAltMSL       = 584.0 // m
	gridSpacing      = 300.0 // m between circle centres
	period           = 60 * time.Second
	metresPerDegLat  = 111320.0
	gravity          = 9.80665
)

// path is one drone's circle.
type path struct {
	lat, lng float64 // centre
	radius   float64 // m
	alt      float64 // m above home
	phase    float64 // starting angle, radians
}

// newPath places drone i of count on a square grid, with radii and altitudes
// varied so neighbours are easy to tell apart on the map.
func newPath(i, count int) path {
	side := int(math.Ceil(math.Sqrt(float64(count))))
	north := float64(i/side) * gridSpacing
	east := float64(i%side) * gridSpacing
	return path{
		lat:    homeLat + north/metresPerDegLat,
		lng:    homeLng + east/(metresPerDegLat*math.Cos(homeLat*math.Pi/180)),
		radius: 50 + float64(i%5)*10,
		alt:    30 + float64(i%4)*5,
		phase:  float64(i) * 0.7,
	}
}

// state is where a drone on its path is after elapsed.
type state struct {
	lat, lng   float64
	vn, ve     float64 // m/s north and east
	headingDeg float64 // 0 to 360, direction of travel
	speed      float64 // m/s
	bank       float64 // rad, for the turn
}

func (p path) at(elapsed time.Duration) state {
	omega := 2 * math.Pi / period.Seconds()
	theta := p.phase + omega*elapsed.Seconds()
	north, east := p.radius*math.Sin(theta), p.radius*math.Cos(theta)
	vn, ve := p.radius*omega*math.Cos(theta), -p.radius*omega*math.Sin(theta) // anticlockwise
	speed := p.radius * omega
	return state{
		lat:        p.lat + north/metresPerDegLat,
		lng:        p.lng + east/(metresPerDegLat*math.Cos(p.lat*math.Pi/180)),
		vn:         vn,
		ve:         ve,
		headingDeg: math.Mod(math.Atan2(ve, vn)*180/math.Pi+360, 360),
		speed:      speed,
		bank:       -math.Atan(speed * speed / (p.radius * gravity)), // anticlockwise: left bank
	}
}

// battery drains 1% every 30 s, down to 5%.
func battery(elapsed time.Duration) (pct int8, mV uint16, consumedMAh int32) {
	p := 100 - elapsed.Seconds()/30
	p = math.Max(p, 5)
	const amps = 14.4
	return int8(p), uint16(10500 + 21*p), int32(amps * elapsed.Hours() * 1000)
}

// messages returns what a drone sends on tick n, n ticks after it started:
// HEARTBEAT and BATTERY_STATUS at 1 Hz, GLOBAL_POSITION_INT and ATTITUDE at
// 5 Hz, SYS_STATUS at 2 Hz (guide step 2.8). Tick 0 starts with the
// HEARTBEAT, so the ingester registers the drone before its first data.
func (p path) messages(n int) []message.Message {
	elapsed := time.Duration(n) * tick
	boot := uint32(elapsed.Milliseconds())
	s := p.at(elapsed)
	pct, mV, mAh := battery(elapsed)

	var out []message.Message
	if n%10 == 0 {
		out = append(out, &ardupilotmega.MessageHeartbeat{
			Type:           ardupilotmega.MAV_TYPE_QUADROTOR,
			Autopilot:      ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA,
			BaseMode:       217, // armed, guided, stabilise, custom mode: as SITL in flight
			CustomMode:     4,   // GUIDED
			SystemStatus:   ardupilotmega.MAV_STATE_ACTIVE,
			MavlinkVersion: 3,
		})
	}
	if n%2 == 0 {
		yaw := s.headingDeg * math.Pi / 180
		if yaw > math.Pi {
			yaw -= 2 * math.Pi
		}
		out = append(out,
			&ardupilotmega.MessageGlobalPositionInt{
				TimeBootMs:  boot,
				Lat:         int32(math.Round(s.lat * 1e7)),
				Lon:         int32(math.Round(s.lng * 1e7)),
				Alt:         int32((homeAltMSL + p.alt) * 1000),
				RelativeAlt: int32(p.alt * 1000),
				Vx:          int16(math.Round(s.vn * 100)),
				Vy:          int16(math.Round(s.ve * 100)),
				Hdg:         uint16(math.Round(s.headingDeg*100)) % 36000,
			},
			&ardupilotmega.MessageAttitude{
				TimeBootMs: boot,
				Roll:       float32(s.bank),
				Pitch:      -0.05,
				Yaw:        float32(yaw),
				Yawspeed:   float32(2 * math.Pi / period.Seconds()),
			})
	}
	if n%5 == 0 {
		out = append(out, &ardupilotmega.MessageSysStatus{
			VoltageBattery:   mV,
			CurrentBattery:   1440, // cA
			BatteryRemaining: pct,
		})
	}
	if n%10 == 0 {
		voltages := [10]uint16{mV}
		for i := 1; i < len(voltages); i++ {
			voltages[i] = math.MaxUint16 // unused cell
		}
		out = append(out, &ardupilotmega.MessageBatteryStatus{
			Voltages:         voltages,
			CurrentBattery:   1440,
			CurrentConsumed:  mAh,
			BatteryRemaining: pct,
			Temperature:      math.MaxInt16, // unknown, as SITL
		})
	}
	return out
}
