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
	onMission
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

	items    []missionItem // the mission held, once complete
	incoming []missionItem // the upload in progress
	want     int           // items expected in it
	item     int           // the item being flown
}

// missionItem is one item of a mission.
type missionItem struct {
	cmd      common.MAV_CMD
	lat, lng float64
	alt      float64
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
		var there bool
		if s, there = goTo(s, pl.path.lat, pl.path.lng, dt); there {
			pl.phase = landing
		}
	case onMission:
		s = pl.flyMission(s, dt)
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

// goTo flies s towards a point and reports whether it got there.
func goTo(s state, lat, lng, dt float64) (state, bool) {
	lngScale := metresPerDegLat * math.Cos(s.lat*math.Pi/180)
	north := (lat - s.lat) * metresPerDegLat
	east := (lng - s.lng) * lngScale
	left := math.Hypot(north, east)
	step := returnSpeed * dt
	if left <= step {
		s.lat, s.lng = lat, lng
		return s, true
	}
	s.vn, s.ve, s.speed = returnSpeed*north/left, returnSpeed*east/left, returnSpeed
	s.lat += step * north / left / metresPerDegLat
	s.lng += step * east / left / lngScale
	s.headingDeg = math.Mod(math.Atan2(east, north)*180/math.Pi+360, 360)
	return s, false
}

// flyMission works through the mission's items, as AUTO does.
func (pl *pilot) flyMission(s state, dt float64) state {
	if pl.item >= len(pl.items) {
		pl.phase = holding
		return s
	}
	it := pl.items[pl.item]
	switch it.cmd {
	case common.MAV_CMD_NAV_TAKEOFF:
		if s.alt < it.alt {
			s.alt = math.Min(s.alt+climbRate*dt, it.alt)
			return s
		}
	case common.MAV_CMD_NAV_WAYPOINT:
		var there bool
		if s, there = goTo(s, it.lat, it.lng, dt); !there {
			return s
		}
		s.alt = it.alt
	case common.MAV_CMD_NAV_RETURN_TO_LAUNCH:
		pl.mode = modeRTL
		pl.phase = returning
		return s
	}
	pl.item++
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

	case common.MAV_CMD_MISSION_START:
		if !pl.armed || len(pl.items) == 0 {
			return ardupilotmega.MAV_RESULT_FAILED
		}
		pl.startMission()
		return ardupilotmega.MAV_RESULT_ACCEPTED
	}
	return ardupilotmega.MAV_RESULT_UNSUPPORTED
}

func (pl *pilot) startMission() {
	pl.mode, pl.phase = modeAuto, onMission
	pl.item = 1 // item 0 is home
}

// missionCount starts an upload and returns the item to ask for.
func (pl *pilot) missionCount(n int) int {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	pl.want, pl.incoming = n, nil
	return 0
}

// missionItem takes an item. It returns the next one to ask for, or done once
// it holds them all. An item out of turn gets the one it needs asked for again.
func (pl *pilot) missionItem(m *ardupilotmega.MessageMissionItemInt) (next int, done bool) {
	pl.mu.Lock()
	defer pl.mu.Unlock()
	if int(m.Seq) == len(pl.incoming) && len(pl.incoming) < pl.want {
		pl.incoming = append(pl.incoming, missionItem{cmd: m.Command, lat: float64(m.X) / 1e7, lng: float64(m.Y) / 1e7, alt: float64(m.Z)})
	}
	if len(pl.incoming) == pl.want { // also a resent last item: its ack was lost
		pl.items = pl.incoming
		return 0, true
	}
	return len(pl.incoming), false
}

func (pl *pilot) setMode(mode uint32) ardupilotmega.MAV_RESULT {
	switch mode {
	case modeGuided:
		if pl.phase == onRoute || pl.phase == returning || pl.phase == landing || pl.phase == onMission {
			pl.phase = holding // stops where it is, as GUIDED does with no target
		}
	case modeAuto:
		if len(pl.items) > 0 && pl.armed {
			pl.startMission()
			return ardupilotmega.MAV_RESULT_ACCEPTED
		}
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
			if rand.Float64() < drop {
				continue
			}
			var answer message.Message
			switch m := f.Msg.(type) {
			case *ardupilotmega.MessageCommandLong:
				if m.TargetSystem != sysID {
					continue
				}
				result := ardupilotmega.MAV_RESULT_TEMPORARILY_REJECTED
				if rand.Float64() >= reject {
					result = pl.command(m)
				}
				answer = &ardupilotmega.MessageCommandAck{Command: m.Command, Result: result, TargetSystem: f.SysID, TargetComponent: f.CompID}
			case *ardupilotmega.MessageMissionCount:
				if m.TargetSystem != sysID {
					continue
				}
				if rand.Float64() < reject {
					answer = &ardupilotmega.MessageMissionAck{TargetSystem: f.SysID, TargetComponent: f.CompID, Type: ardupilotmega.MAV_MISSION_DENIED}
					break
				}
				answer = &ardupilotmega.MessageMissionRequestInt{TargetSystem: f.SysID, TargetComponent: f.CompID, Seq: uint16(pl.missionCount(int(m.Count)))}
			case *ardupilotmega.MessageMissionItemInt:
				if m.TargetSystem != sysID {
					continue
				}
				next, done := pl.missionItem(m)
				if done {
					answer = &ardupilotmega.MessageMissionAck{TargetSystem: f.SysID, TargetComponent: f.CompID, Type: ardupilotmega.MAV_MISSION_ACCEPTED}
				} else {
					answer = &ardupilotmega.MessageMissionRequestInt{TargetSystem: f.SysID, TargetComponent: f.CompID, Seq: uint16(next)}
				}
			default:
				continue
			}
			if err := r.send(answer); err != nil {
				log.Printf("drone %d: answer: %v", sysID, err)
			}
		}
	}
}
