package main

import (
	"fmt"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/alert"
	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/state"
)

// recover is how far above the low-battery level a drone must climb back
// before its alert clears, so a reading wobbling at the level does not flap.
const recoverBy = 5

// rules turns telemetry into alerts. Each alert is raised once, when its
// condition starts, and cleared once, when it ends.
type rules struct {
	lowBattery int           // percent at or below which to alert
	timeout    time.Duration // no heartbeat for this long: link lost
	stale      time.Duration // nothing heard for this long: forget the drone
	emit       func(alert.Alert)

	mu     sync.Mutex
	drones map[int64]*watched
	fence  []alert.Point
}

type watched struct {
	lastHeartbeat, lastSeen time.Time
	pos                     *alert.Point
	active                  map[string]alert.Alert
}

func newRules(lowBattery int, timeout, stale time.Duration, emit func(alert.Alert)) *rules {
	return &rules{lowBattery: lowBattery, timeout: timeout, stale: stale, emit: emit, drones: map[int64]*watched{}}
}

// observe applies one message. Only three types matter, so the payload is
// decoded only for those.
func (r *rules) observe(t schema.Telemetry) {
	switch t.Msg {
	case "HEARTBEAT", "SYS_STATUS", "GLOBAL_POSITION_INT":
	default:
		r.seen(t.DroneID, t.ReceivedAt)
		return
	}
	msg, err := t.Message()
	if err != nil {
		return
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	w := r.drone(t.DroneID)
	if t.ReceivedAt.After(w.lastSeen) {
		w.lastSeen = t.ReceivedAt
	}
	switch m := msg.(type) {
	case *ardupilotmega.MessageHeartbeat:
		if t.ReceivedAt.After(w.lastHeartbeat) {
			w.lastHeartbeat = t.ReceivedAt
		}
		r.set(t.DroneID, w, alert.LinkLost, false, "Heartbeat back", t.ReceivedAt)
	case *ardupilotmega.MessageSysStatus:
		p := state.BatteryPercent(m.BatteryRemaining)
		if p == nil {
			return
		}
		_, low := w.active[alert.LowBattery]
		switch {
		case *p <= r.lowBattery:
			r.set(t.DroneID, w, alert.LowBattery, true, fmt.Sprintf("Battery at %d%%", *p), t.ReceivedAt)
		case low && *p >= r.lowBattery+recoverBy:
			r.set(t.DroneID, w, alert.LowBattery, false, fmt.Sprintf("Battery back to %d%%", *p), t.ReceivedAt)
		}
	case *ardupilotmega.MessageGlobalPositionInt:
		if m.Lat == 0 && m.Lon == 0 {
			return // no GPS fix yet
		}
		w.pos = &alert.Point{Lat: float64(m.Lat) / 1e7, Lon: float64(m.Lon) / 1e7}
		r.checkFence(t.DroneID, w, t.ReceivedAt)
	}
}

func (r *rules) seen(id int64, at time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if w := r.drone(id); at.After(w.lastSeen) {
		w.lastSeen = at
	}
}

// drone returns a drone's state. The caller holds r.mu.
func (r *rules) drone(id int64) *watched {
	w, ok := r.drones[id]
	if !ok {
		w = &watched{active: map[string]alert.Alert{}}
		r.drones[id] = w
	}
	return w
}

// checkFence raises or clears a drone's breach. The caller holds r.mu.
func (r *rules) checkFence(id int64, w *watched, at time.Time) {
	if w.pos == nil {
		return
	}
	outside := len(r.fence) >= 3 && !alert.Contains(r.fence, *w.pos)
	msg := "Back inside the geofence"
	if outside {
		msg = "Outside the geofence"
	}
	r.set(id, w, alert.Geofence, outside, msg, at)
}

// set changes an alert's state, emitting only on a change. The caller holds r.mu.
func (r *rules) set(id int64, w *watched, kind string, raised bool, msg string, at time.Time) {
	if _, on := w.active[kind]; on == raised {
		return
	}
	a := alert.Alert{DroneID: id, Kind: kind, Raised: raised, Message: msg, At: at}
	if raised {
		w.active[kind] = a
	} else {
		delete(w.active, kind)
	}
	r.emit(a)
}

// tick raises link lost for drones whose heartbeats stopped, and forgets
// drones silent for longer than stale, as the map hides them.
func (r *rules) tick(now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	for id, w := range r.drones {
		if now.Sub(w.lastSeen) > r.stale {
			delete(r.drones, id)
			continue
		}
		if !w.lastHeartbeat.IsZero() && now.Sub(w.lastHeartbeat) > r.timeout {
			r.set(id, w, alert.LinkLost, true, fmt.Sprintf("No heartbeat for %.0f s", now.Sub(w.lastHeartbeat).Seconds()), now)
		}
	}
}

// setFence replaces the geofence and checks every drone against it at once.
func (r *rules) setFence(polygon []alert.Point, now time.Time) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.fence = polygon
	for id, w := range r.drones {
		r.checkFence(id, w, now)
	}
}

// active returns the alerts raised now.
func (r *rules) active() []alert.Alert {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := []alert.Alert{}
	for _, w := range r.drones {
		for _, a := range w.active {
			out = append(out, a)
		}
	}
	return out
}
