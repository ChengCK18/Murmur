package main

import (
	"math"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

// tick is the base rate: every rate below is a whole number of ticks.
const tick = 100 * time.Millisecond

// Home and layout: each drone flies its route in its own cell of a square
// grid around ArduPilot's SITL home.
const (
	homeLat, homeLng = -35.363262, 149.165237
	homeAltMSL       = 584.0 // m
	gridSpacing      = 300.0 // m between cell centres
	metresPerDegLat  = 111320.0
	gravity          = 9.80665
)

// path is one drone's flight: a route around the centre of its grid cell.
type path struct {
	lat, lng float64 // cell centre
	alt      float64 // m above home
	route    route
}

// route gives a drone's position relative to its cell centre after elapsed.
type route interface {
	at(elapsed time.Duration) local
}

// local is a position and motion in metres north and east of a cell centre.
type local struct {
	north, east float64
	vn, ve      float64 // m/s
	yawRate     float64 // rad/s, positive clockwise seen from above
}

// newPath places drone i of count on a square grid. Drones take turns at
// the three routes of guide step 4.1 (circle, line, lawnmower survey), with
// sizes, speeds, altitudes and starting points varied so neighbours are easy
// to tell apart on the map.
func newPath(i, count int) path {
	side := int(math.Ceil(math.Sqrt(float64(count))))
	north := float64(i/side) * gridSpacing
	east := float64(i%side) * gridSpacing
	speed := 6 + float64(i%4) // m/s
	start := math.Mod(float64(i)*0.618, 1)
	var r route
	switch i % 3 {
	case 0:
		r = circle{radius: 50 + float64(i%5)*10, period: 60 * time.Second, phase: start * 2 * math.Pi}
	case 1:
		r = newLine(160+float64(i%5)*20, float64(i)*37*math.Pi/180, speed, start)
	default:
		r = newLawnmower(160+float64(i%5)*10, 6, 30, speed, start)
	}
	return path{
		lat:   homeLat + north/metresPerDegLat,
		lng:   homeLng + east/(metresPerDegLat*math.Cos(homeLat*math.Pi/180)),
		alt:   30 + float64(i%4)*5,
		route: r,
	}
}

// circle flies anticlockwise around the cell centre, once per period.
type circle struct {
	radius float64       // m
	period time.Duration // one lap
	phase  float64       // starting angle, radians from east
}

func (c circle) at(elapsed time.Duration) local {
	omega := 2 * math.Pi / c.period.Seconds()
	theta := c.phase + omega*elapsed.Seconds()
	return local{
		north:   c.radius * math.Sin(theta),
		east:    c.radius * math.Cos(theta),
		vn:      c.radius * omega * math.Cos(theta),
		ve:      -c.radius * omega * math.Sin(theta),
		yawRate: -omega, // anticlockwise
	}
}

// loop flies a closed polyline at a steady speed, turning on the spot at
// each corner, as a multicopter does between waypoints.
type loop struct {
	points []vec // m north and east of the cell centre
	cum    []float64
	speed  float64 // m/s
	start  float64 // m along the loop at elapsed 0
}

type vec struct{ north, east float64 }

func newLoop(points []vec, speed, startFrac float64) loop {
	l := loop{points: points, speed: speed, cum: make([]float64, len(points)+1)}
	for i, p := range points {
		q := points[(i+1)%len(points)]
		l.cum[i+1] = l.cum[i] + math.Hypot(q.north-p.north, q.east-p.east)
	}
	l.start = startFrac * l.length()
	return l
}

func (l loop) length() float64 { return l.cum[len(l.points)] }

func (l loop) at(elapsed time.Duration) local {
	d := math.Mod(l.start+l.speed*elapsed.Seconds(), l.length())
	i := 0
	for i < len(l.points)-1 && d >= l.cum[i+1] {
		i++
	}
	p, q := l.points[i], l.points[(i+1)%len(l.points)]
	seg := l.cum[i+1] - l.cum[i]
	f := (d - l.cum[i]) / seg
	dn, de := (q.north-p.north)/seg, (q.east-p.east)/seg
	return local{
		north: p.north + f*(q.north-p.north),
		east:  p.east + f*(q.east-p.east),
		vn:    dn * l.speed,
		ve:    de * l.speed,
	}
}

// newLine flies back and forth along a line through the cell centre, angle
// radians clockwise from north.
func newLine(length, angle, speed, startFrac float64) loop {
	n, e := length/2*math.Cos(angle), length/2*math.Sin(angle)
	return newLoop([]vec{{-n, -e}, {n, e}}, speed, startFrac)
}

// newLawnmower flies a survey pattern: rows west to east and back, spacing
// metres apart, then straight back down the west edge to the first row.
func newLawnmower(width float64, rows int, spacing, speed, startFrac float64) loop {
	south := -float64(rows-1) * spacing / 2
	var pts []vec
	for r := range rows {
		n := south + float64(r)*spacing
		w, e := vec{n, -width / 2}, vec{n, width / 2}
		if r%2 == 0 {
			pts = append(pts, w, e)
		} else {
			pts = append(pts, e, w)
		}
	}
	if rows%2 == 1 { // the last row ends in the east: cross back first
		pts = append(pts, vec{pts[len(pts)-1].north, -width / 2})
	}
	return newLoop(pts, speed, startFrac)
}

// state is where a drone on its path is after elapsed.
type state struct {
	lat, lng   float64
	vn, ve     float64 // m/s north and east
	headingDeg float64 // 0 to 360, direction of travel
	speed      float64 // m/s
	yawRate    float64 // rad/s, positive clockwise
	bank       float64 // rad, for the turn
	alt        float64 // m above home
	armed      bool
	mode       uint32 // ArduPilot Copter mode number
}

func (p path) at(elapsed time.Duration) state {
	l := p.route.at(elapsed)
	speed := math.Hypot(l.vn, l.ve)
	return state{
		lat:        p.lat + l.north/metresPerDegLat,
		lng:        p.lng + l.east/(metresPerDegLat*math.Cos(p.lat*math.Pi/180)),
		vn:         l.vn,
		ve:         l.ve,
		headingDeg: math.Mod(math.Atan2(l.ve, l.vn)*180/math.Pi+360, 360),
		speed:      speed,
		yawRate:    l.yawRate,
		bank:       math.Atan(speed * l.yawRate / gravity), // a coordinated turn
		alt:        p.alt,
		armed:      true,
		mode:       modeAuto, // following its route
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
// 10 a second (guide step 4.1). HEARTBEAT, SYS_STATUS and BATTERY_STATUS at
// 1 Hz, GLOBAL_POSITION_INT at 5 Hz as SITL sends it, and ATTITUDE at 2 Hz
// (SITL: 5 Hz; the map draws heading from the position). Tick 0 starts with
// the HEARTBEAT, so the ingester registers the drone before its first data.
func (p path) messages(n int) []message.Message {
	return frames(n, p.at(time.Duration(n)*tick))
}

// frames is what a drone in state s sends on tick n.
func frames(n int, s state) []message.Message {
	elapsed := time.Duration(n) * tick
	boot := uint32(elapsed.Milliseconds())
	pct, mV, mAh := battery(elapsed)

	var out []message.Message
	if n%10 == 0 {
		out = append(out, &ardupilotmega.MessageHeartbeat{
			Type:           ardupilotmega.MAV_TYPE_QUADROTOR,
			Autopilot:      ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA,
			BaseMode:       baseMode(s.armed),
			CustomMode:     s.mode,
			SystemStatus:   systemStatus(s.armed),
			MavlinkVersion: 3,
		})
	}
	if n%2 == 0 {
		out = append(out, &ardupilotmega.MessageGlobalPositionInt{
			TimeBootMs:  boot,
			Lat:         int32(math.Round(s.lat * 1e7)),
			Lon:         int32(math.Round(s.lng * 1e7)),
			Alt:         int32((homeAltMSL + s.alt) * 1000),
			RelativeAlt: int32(s.alt * 1000),
			Vx:          int16(math.Round(s.vn * 100)),
			Vy:          int16(math.Round(s.ve * 100)),
			Hdg:         uint16(math.Round(s.headingDeg*100)) % 36000,
		})
	}
	if n%5 == 0 {
		yaw := s.headingDeg * math.Pi / 180
		if yaw > math.Pi {
			yaw -= 2 * math.Pi
		}
		out = append(out, &ardupilotmega.MessageAttitude{
			TimeBootMs: boot,
			Roll:       float32(s.bank),
			Pitch:      -0.05,
			Yaw:        float32(yaw),
			Yawspeed:   float32(s.yawRate),
		})
	}
	if n%10 == 0 {
		voltages := [10]uint16{mV}
		for i := 1; i < len(voltages); i++ {
			voltages[i] = math.MaxUint16 // unused cell
		}
		out = append(out,
			&ardupilotmega.MessageSysStatus{
				VoltageBattery:   mV,
				CurrentBattery:   1440, // cA
				BatteryRemaining: pct,
			},
			&ardupilotmega.MessageBatteryStatus{
				Voltages:         voltages,
				CurrentConsumed:  mAh,
				CurrentBattery:   1440,
				BatteryRemaining: pct,
				Temperature:      math.MaxInt16, // unknown, as SITL
			})
	}
	return out
}

// baseMode is HEARTBEAT.base_mode as SITL sends it: guided, stabilise and
// custom mode, plus the armed flag.
func baseMode(armed bool) ardupilotmega.MAV_MODE_FLAG {
	m := ardupilotmega.MAV_MODE_FLAG_GUIDED_ENABLED | ardupilotmega.MAV_MODE_FLAG_STABILIZE_ENABLED |
		ardupilotmega.MAV_MODE_FLAG_MANUAL_INPUT_ENABLED | ardupilotmega.MAV_MODE_FLAG_CUSTOM_MODE_ENABLED
	if armed {
		m |= ardupilotmega.MAV_MODE_FLAG_SAFETY_ARMED
	}
	return m
}

func systemStatus(armed bool) ardupilotmega.MAV_STATE {
	if armed {
		return ardupilotmega.MAV_STATE_ACTIVE
	}
	return ardupilotmega.MAV_STATE_STANDBY
}
