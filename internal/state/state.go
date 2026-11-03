// Package state merges telemetry into each drone's latest state: what the map
// shows. The gateway builds it from the live feed and from the consumer's
// Redis hashes; both apply the same messages the same way.
package state

import (
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
)

// Link values.
const (
	LinkOK   = "ok"
	LinkLost = "lost"
)

// Drone is the latest state of one drone, as sent to browsers. Fields the
// drone has not reported yet are left out.
type Drone struct {
	TenantID int64  `json:"tenant_id"`
	DroneID  int64  `json:"drone_id"`
	SysID    uint8  `json:"sys_id"`
	Mode     string `json:"mode,omitempty"`
	Armed    bool   `json:"armed"`
	Link     string `json:"link"` // LinkOK or LinkLost, from the last heartbeat

	Position *Position `json:"position,omitempty"`
	Battery  *Battery  `json:"battery,omitempty"`

	LastSeen      time.Time `json:"last_seen"`
	LastHeartbeat time.Time `json:"last_heartbeat"`

	applied map[string]time.Time // newest received_at applied, per message type
}

// Position comes from GLOBAL_POSITION_INT.
type Position struct {
	Lat        float64  `json:"lat"`         // degrees
	Lon        float64  `json:"lon"`         // degrees
	AltMSL     float64  `json:"alt_m"`       // above mean sea level
	RelAlt     float64  `json:"rel_alt_m"`   // above home
	Speed      float64  `json:"speed_ms"`    // ground speed
	HeadingDeg *float64 `json:"heading_deg"` // nil when the drone reports it as unknown
}

// PositionOf converts GLOBAL_POSITION_INT's raw units: degrees × 10⁷,
// millimetres, cm/s north and east, centidegrees (UINT16_MAX: unknown).
func PositionOf(m *ardupilotmega.MessageGlobalPositionInt) Position {
	p := Position{
		Lat:    float64(m.Lat) / 1e7,
		Lon:    float64(m.Lon) / 1e7,
		AltMSL: float64(m.Alt) / 1000,
		RelAlt: float64(m.RelativeAlt) / 1000,
		Speed:  math.Hypot(float64(m.Vx), float64(m.Vy)) / 100,
	}
	if m.Hdg != math.MaxUint16 {
		h := float64(m.Hdg) / 100
		p.HeadingDeg = &h
	}
	return p
}

// Battery comes from SYS_STATUS and BATTERY_STATUS.
type Battery struct {
	Percent     *int     `json:"percent"`                // nil when unknown
	Voltage     *float64 `json:"voltage_v,omitempty"`    // SYS_STATUS
	ConsumedMAh *int     `json:"consumed_mah,omitempty"` // BATTERY_STATUS
}

// BatteryPercent converts battery_remaining, where -1 means unknown.
func BatteryPercent(v int8) *int {
	if v < 0 {
		return nil
	}
	p := int(v)
	return &p
}

// New returns the state of a drone that has reported nothing yet.
func New(tenantID, droneID int64) *Drone {
	return &Drone{TenantID: tenantID, DroneID: droneID, Link: LinkLost}
}

// Apply merges one message and reports whether it changed the state. A
// message older than the last one of its type already applied is ignored, so
// the order messages arrive in does not matter.
func (d *Drone) Apply(t schema.Telemetry) (bool, error) {
	if prev, ok := d.applied[t.Msg]; ok && !t.ReceivedAt.After(prev) {
		return false, nil
	}
	msg, err := t.Message()
	if err != nil {
		return false, err
	}
	switch m := msg.(type) {
	case *ardupilotmega.MessageHeartbeat:
		if m.Autopilot == ardupilotmega.MAV_AUTOPILOT_INVALID {
			return false, nil // a camera or gimbal, not the flight controller
		}
		d.Mode, d.Armed = mavlink.ModeName(m), mavlink.IsArmed(m)
		d.LastHeartbeat = t.ReceivedAt
	case *ardupilotmega.MessageGlobalPositionInt:
		p := PositionOf(m)
		d.Position = &p
	case *ardupilotmega.MessageSysStatus:
		b := d.battery()
		b.Percent = BatteryPercent(m.BatteryRemaining)
		v := float64(m.VoltageBattery) / 1000
		b.Voltage = &v
	case *ardupilotmega.MessageBatteryStatus:
		b := d.battery()
		if p := BatteryPercent(m.BatteryRemaining); p != nil {
			b.Percent = p
		}
		if m.CurrentConsumed >= 0 { // -1: unknown
			mah := int(m.CurrentConsumed)
			b.ConsumedMAh = &mah
		}
	default:
		return false, nil // not shown on the map
	}
	if d.applied == nil {
		d.applied = map[string]time.Time{}
	}
	d.applied[t.Msg] = t.ReceivedAt
	d.SysID = t.SysID
	if t.ReceivedAt.After(d.LastSeen) {
		d.LastSeen = t.ReceivedAt
	}
	return true, nil
}

func (d *Drone) battery() *Battery {
	if d.Battery == nil {
		d.Battery = &Battery{}
	}
	return d.Battery
}

// UpdateLink sets Link from the time of the last heartbeat and reports
// whether it changed.
func (d *Drone) UpdateLink(now time.Time, timeout time.Duration) bool {
	link := LinkLost
	if !d.LastHeartbeat.IsZero() && now.Sub(d.LastHeartbeat) <= timeout {
		link = LinkOK
	}
	changed := d.Link != link
	d.Link = link
	return changed
}

// Clone returns a copy that shares nothing with d.
func (d *Drone) Clone() *Drone {
	c := *d
	if d.Position != nil {
		p := *d.Position
		if p.HeadingDeg != nil {
			h := *p.HeadingDeg
			p.HeadingDeg = &h
		}
		c.Position = &p
	}
	if d.Battery != nil {
		b := *d.Battery
		c.Battery = &b // the pointers inside are never written through, only replaced
	}
	c.applied = make(map[string]time.Time, len(d.applied))
	for k, v := range d.applied {
		c.applied[k] = v
	}
	return &c
}

// FromRedis rebuilds a drone's state from its Redis hash
// (drone:<tenant_id>:<drone_id>), which the consumer fills with the latest
// message of each type as schema.Telemetry JSON.
func FromRedis(tenantID, droneID int64, hash map[string]string) (*Drone, error) {
	d := New(tenantID, droneID)
	for field, value := range hash {
		if strings.Contains(field, ":") || field == "last_seen" {
			continue // bookkeeping: <MSG>:us, last_seen, last_seen:us
		}
		var t schema.Telemetry
		if err := json.Unmarshal([]byte(value), &t); err != nil {
			return nil, fmt.Errorf("state: drone %d field %s: %w", droneID, field, err)
		}
		if _, err := d.Apply(t); err != nil {
			return nil, fmt.Errorf("state: drone %d field %s: %w", droneID, field, err)
		}
	}
	return d, nil
}
