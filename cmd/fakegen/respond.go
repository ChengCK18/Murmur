package main

import (
	"errors"
	"log"
	"math"
	"math/rand/v2"
	"net"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v3/pkg/frame"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/mavlink"
)

// ArduPilot Copter mode numbers.
const (
	modeAuto   = 3
	modeGuided = 4
	modeRTL    = 6
	modeLand   = 9
)

const (
	climbRate   = 2.5  // m/s
	descentRate = 1.5  // m/s
	returnSpeed = 10.0 // m/s
)

// phase is what a drone is doing. It starts on its route, already flying.
type phase int

const (
	onRoute phase = iota
	climbing
	holding
	returning
	landing
	grounded
)

// pilot flies one drone: its route until a command says otherwise (guide step
// 5.3). The answers copy what SITL does, so the platform cannot tell them apart.
type pilot struct {
	path path

	mu     sync.Mutex
	phase  phase
	armed  bool
	mode   uint32
	last   state
	target float64       // climb to this height
	clock  time.Duration // the elapsed time of the last at
}

func newPilot(p path) *pilot {
	return &pilot{path: p, armed: true, mode: modeAuto}
}

// at moves the drone on to elapsed and returns its state.
func (pl *pilot) at(elapsed time.Duration) state {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	dt := (elapsed - pl.clock).Seconds()
	pl.clock = elapsed

	if pl.phase == onRoute {
		pl.last = pl.path.at(elapsed)
		return pl.last
	}
	s := pl.last
	s.vn, s.ve, s.speed, s.yawRate, s.bank = 0, 0, 0, 0, 0
	switch pl.phase {
	case climbing:
		s.alt += climbRate * dt
		if s.alt >= pl.target {
			s.alt = pl.target
			pl.phase = holding
		}
	case returning:
		s = pl.goHome(s, dt)
	case landing:
		s.alt -= descentRate * dt
		if s.alt <= 0 {
			s.alt = 0
			pl.phase = grounded
			pl.armed = false // ArduPilot disarms after landing
		}
	}
	s.armed, s.mode = pl.armed, pl.mode
	pl.last = s
	return s
}

// goHome flies s towards the centre of the drone's cell, and lands there.
func (pl *pilot) goHome(s state, dt float64) state {
	lngScale := metresPerDegLat * math.Cos(s.lat*math.Pi/180)
	north := (pl.path.lat - s.lat) * metresPerDegLat
	east := (pl.path.lng - s.lng) * lngScale
	left := math.Hypot(north, east)
	step := returnSpeed * dt
	if left <= step {
		s.lat, s.lng = pl.path.lat, pl.path.lng
		pl.phase = landing
		return s
	}
	s.vn, s.ve, s.speed = returnSpeed*north/left, returnSpeed*east/left, returnSpeed
	s.lat += step * north / left / metresPerDegLat
	s.lng += step * east / left / lngScale
	s.headingDeg = math.Mod(math.Atan2(east, north)*180/math.Pi+360, 360)
	return s
}

func (pl *pilot) flying() bool { return pl.phase != grounded }

// command acts on a COMMAND_LONG. Repeats are harmless: a resent command finds
// the drone already doing what it asks.
func (pl *pilot) command(m *ardupilotmega.MessageCommandLong) ardupilotmega.MAV_RESULT {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	switch m.Command {
	case common.MAV_CMD_COMPONENT_ARM_DISARM:
		if m.Param1 == 1 {
			pl.armed = true
			return ardupilotmega.MAV_RESULT_ACCEPTED
		}
		if pl.flying() {
			return ardupilotmega.MAV_RESULT_FAILED // as ArduPilot, in the air
		}
		pl.armed = false
		return ardupilotmega.MAV_RESULT_ACCEPTED

	case common.MAV_CMD_DO_SET_MODE:
		return pl.setMode(uint32(m.Param2))

	case common.MAV_CMD_NAV_TAKEOFF:
		switch {
		case !pl.armed || pl.mode != modeGuided:
			return ardupilotmega.MAV_RESULT_DENIED
		case pl.phase == climbing: // a resend
			return ardupilotmega.MAV_RESULT_ACCEPTED
		case pl.phase != grounded:
			return ardupilotmega.MAV_RESULT_FAILED
		}
		pl.phase, pl.target = climbing, float64(m.Param7)
		return ardupilotmega.MAV_RESULT_ACCEPTED

	case common.MAV_CMD_NAV_RETURN_TO_LAUNCH:
		return pl.setMode(modeRTL)

	case common.MAV_CMD_NAV_LAND:
		return pl.setMode(modeLand)
	}
	return ardupilotmega.MAV_RESULT_UNSUPPORTED
}

func (pl *pilot) setMode(mode uint32) ardupilotmega.MAV_RESULT {
	switch mode {
	case modeGuided:
		if pl.phase == onRoute || pl.phase == returning || pl.phase == landing {
			pl.phase = holding // stops where it is, as GUIDED does with no target
		}
	case modeAuto:
		if pl.phase != onRoute {
			return ardupilotmega.MAV_RESULT_FAILED // ArduPilot refuses AUTO with no mission
		}
	case modeRTL, modeLand:
		if !pl.armed || !pl.flying() {
			return ardupilotmega.MAV_RESULT_DENIED
		}
		if mode == modeRTL {
			pl.phase = returning
		} else {
			pl.phase = landing
		}
	default:
		return ardupilotmega.MAV_RESULT_UNSUPPORTED
	}
	pl.mode = mode
	return ardupilotmega.MAV_RESULT_ACCEPTED
}

// radio lets the flight loop and the responder share a frame writer, and so a
// sequence counter.
type radio struct {
	mu sync.Mutex
	w  *frame.Writer
}

func (r *radio) send(m message.Message) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.w.WriteMessage(m)
}

// respond answers commands on a drone's socket until it is closed.
func respond(conn *net.UDPConn, dec *mavlink.Decoder, r *radio, pl *pilot, sysID uint8, drop, reject float64) {
	buf := make([]byte, 2048)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if !errors.Is(err, net.ErrClosed) {
				log.Printf("drone %d: read: %v", sysID, err)
			}
			return
		}
		frames, _ := dec.Decode(time.Now(), src, buf[:n])
		for _, f := range frames {
			m, ok := f.Msg.(*ardupilotmega.MessageCommandLong)
			if !ok || m.TargetSystem != sysID || rand.Float64() < drop {
				continue
			}
			result := ardupilotmega.MAV_RESULT_TEMPORARILY_REJECTED
			if rand.Float64() >= reject {
				result = pl.command(m)
			}
			ack := &ardupilotmega.MessageCommandAck{Command: m.Command, Result: result, TargetSystem: f.SysID, TargetComponent: f.CompID}
			if err := r.send(ack); err != nil {
				log.Printf("drone %d: ack: %v", sysID, err)
			}
		}
	}
}
