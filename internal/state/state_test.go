package state

import (
	"encoding/json"
	"net/netip"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/tlog"
)

// flight returns the drone's messages from flight.tlog as published.
func flight(t *testing.T) []schema.Telemetry {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/flight.tlog")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := mavlink.NewDecoder()
	var out []schema.Telemetry
	for _, rec := range recs {
		frames, _ := d.Decode(rec.Time, netip.AddrPort{}, rec.Frame)
		for _, r := range frames {
			if r.SysID == 1 {
				m, err := schema.FromReceived(1, 1, r)
				if err != nil {
					t.Fatal(err)
				}
				out = append(out, m)
			}
		}
	}
	return out
}

func TestFlightEndState(t *testing.T) {
	msgs := flight(t)
	d := New(1, 1)
	for _, m := range msgs {
		if _, err := d.Apply(m); err != nil {
			t.Fatal(err)
		}
	}
	last := msgs[len(msgs)-1].ReceivedAt
	d.UpdateLink(last, 3*time.Second)
	// The values the ingester's console shows at the end of the flight
	// (cmd/ingester tests), cross-checked with pymavlink.
	p := d.Position
	if d.Mode != "AUTO" || d.Armed || d.Link != LinkOK || p == nil ||
		round(p.Lat, 5) != -35.36326 || round(p.Lon, 5) != 149.16524 || round(p.RelAlt, 1) != 0 ||
		p.HeadingDeg == nil || int(*p.HeadingDeg+0.5) != 180 || *d.Battery.Percent != 72 {
		js, _ := json.Marshal(d)
		t.Errorf("end state %s", js)
	}
	if d.Battery.Voltage == nil || d.Battery.ConsumedMAh == nil || *d.Battery.ConsumedMAh != 899 {
		t.Errorf("battery %+v", d.Battery)
	}
	if d.LastSeen.Before(last.Add(-time.Second)) {
		t.Errorf("last seen %v, last message %v", d.LastSeen, last)
	}
}

func round(v float64, places int) float64 {
	p := 1.0
	for range places {
		p *= 10
	}
	return float64(int64(v*p+copysign(0.5, v))) / p
}

func copysign(a, b float64) float64 {
	if b < 0 {
		return -a
	}
	return a
}

func tel(t *testing.T, at time.Time, m message.Message) schema.Telemetry {
	t.Helper()
	s, err := schema.FromReceived(1, 7, mavlink.Received{ReceivedAt: at, SysID: 3, CompID: 1, Msg: m})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func TestOrderDoesNotMatter(t *testing.T) {
	t0 := time.Unix(1793516806, 0)
	newer := tel(t, t0.Add(time.Second), &ardupilotmega.MessageSysStatus{BatteryRemaining: 70, VoltageBattery: 12000})
	older := tel(t, t0, &ardupilotmega.MessageSysStatus{BatteryRemaining: 90, VoltageBattery: 12600})
	d := New(1, 7)
	if changed, _ := d.Apply(newer); !changed {
		t.Error("first message did not change the state")
	}
	if changed, _ := d.Apply(older); changed || *d.Battery.Percent != 70 {
		t.Errorf("a late older message changed the state to %d%%", *d.Battery.Percent)
	}
	if changed, _ := d.Apply(newer); changed {
		t.Error("the same message again changed the state")
	}
	if d.SysID != 3 || !d.LastSeen.Equal(newer.ReceivedAt) {
		t.Errorf("sys %d last seen %v", d.SysID, d.LastSeen)
	}
}

func TestUnknownValues(t *testing.T) {
	t0 := time.Unix(1793516806, 0)
	d := New(1, 7)
	d.Apply(tel(t, t0, &ardupilotmega.MessageGlobalPositionInt{Lat: 1, Hdg: 65535}))                    //nolint:errcheck
	d.Apply(tel(t, t0, &ardupilotmega.MessageSysStatus{BatteryRemaining: -1}))                          //nolint:errcheck
	d.Apply(tel(t, t0, &ardupilotmega.MessageBatteryStatus{BatteryRemaining: -1, CurrentConsumed: -1})) //nolint:errcheck
	if d.Position.HeadingDeg != nil || d.Battery.Percent != nil || d.Battery.ConsumedMAh != nil {
		js, _ := json.Marshal(d)
		t.Errorf("unknown values reported: %s", js)
	}
	// A camera's heartbeat says nothing about the flight controller.
	if changed, _ := d.Apply(tel(t, t0, &ardupilotmega.MessageHeartbeat{Autopilot: ardupilotmega.MAV_AUTOPILOT_INVALID, CustomMode: 6})); changed || d.Mode != "" {
		t.Errorf("gimbal heartbeat set mode %q", d.Mode)
	}
	// Types the map does not show leave the state alone.
	if changed, _ := d.Apply(tel(t, t0, &ardupilotmega.MessageAttitude{})); changed {
		t.Error("ATTITUDE changed the state")
	}
}

func TestNoFixIsNoPosition(t *testing.T) {
	// Before GPS lock a drone reports 0, 0, which is not a place it has been.
	t0 := time.Unix(1793516806, 0)
	d := New(1, 7)
	if changed, _ := d.Apply(tel(t, t0, &ardupilotmega.MessageGlobalPositionInt{})); changed || d.Position != nil {
		t.Errorf("position %+v from a message with no fix", d.Position)
	}
	// Losing the fix keeps the last known position.
	d.Apply(tel(t, t0.Add(time.Second), &ardupilotmega.MessageGlobalPositionInt{Lat: -353660000, Lon: 1491652370})) //nolint:errcheck
	d.Apply(tel(t, t0.Add(2*time.Second), &ardupilotmega.MessageGlobalPositionInt{}))                               //nolint:errcheck
	if d.Position == nil || d.Position.Lat != -35.366 {
		t.Errorf("position %+v after losing the fix", d.Position)
	}
}

func TestLink(t *testing.T) {
	t0 := time.Unix(1793516806, 0)
	d := New(1, 7)
	if d.Link != LinkLost || d.UpdateLink(t0, 3*time.Second) {
		t.Error("a drone never heard from is not lost")
	}
	d.Apply(tel(t, t0, &ardupilotmega.MessageHeartbeat{Autopilot: ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA})) //nolint:errcheck
	steps := []struct {
		after   time.Duration
		link    string
		changed bool
	}{{time.Second, LinkOK, true}, {3 * time.Second, LinkOK, false}, {3100 * time.Millisecond, LinkLost, true}}
	for _, s := range steps {
		if changed := d.UpdateLink(t0.Add(s.after), 3*time.Second); d.Link != s.link || changed != s.changed {
			t.Errorf("after %v: %s changed %v, want %s %v", s.after, d.Link, changed, s.link, s.changed)
		}
	}
}

func TestFromRedis(t *testing.T) {
	// The hash the consumer writes: the newest message of each type plus
	// bookkeeping fields. Rebuilt, it matches applying the whole flight.
	msgs := flight(t)
	live := New(1, 1)
	hash := map[string]string{"last_seen": "x", "last_seen:us": "1"}
	for _, m := range msgs {
		live.Apply(m) //nolint:errcheck
		js, _ := json.Marshal(m)
		hash[m.Msg] = string(js) // later ones overwrite: the newest of each type
		hash[m.Msg+":us"] = "1"
	}
	got, err := FromRedis(1, 1, hash)
	if err != nil {
		t.Fatal(err)
	}
	a, _ := json.Marshal(got)
	b, _ := json.Marshal(live)
	if string(a) != string(b) {
		t.Errorf("from Redis\n %s\nlive\n %s", a, b)
	}
	if _, err := FromRedis(1, 1, map[string]string{"HEARTBEAT": "{"}); err == nil {
		t.Error("bad JSON accepted")
	}
}

func TestCloneIsIndependent(t *testing.T) {
	t0 := time.Unix(1793516806, 0)
	d := New(1, 7)
	d.Apply(tel(t, t0, &ardupilotmega.MessageGlobalPositionInt{Lat: 1e7, Hdg: 9000})) //nolint:errcheck
	c := d.Clone()
	d.Apply(tel(t, t0.Add(time.Second), &ardupilotmega.MessageGlobalPositionInt{Lat: 2e7, Hdg: 18000})) //nolint:errcheck
	if c.Position.Lat != 1 || *c.Position.HeadingDeg != 90 {
		t.Errorf("clone changed with the original: %+v", c.Position)
	}
}
