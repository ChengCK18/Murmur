package main

import (
	"net/netip"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/alert"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
)

var t0 = time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)

type sink struct{ got []alert.Alert }

func (s *sink) emit(a alert.Alert) { s.got = append(s.got, a) }

func tel(t *testing.T, at time.Duration, m message.Message) schema.Telemetry {
	t.Helper()
	tm, err := schema.FromReceived(1, 7, mavlink.Received{ReceivedAt: t0.Add(at), Src: netip.AddrPort{}, SysID: 1, CompID: 1, Msg: m})
	if err != nil {
		t.Fatal(err)
	}
	return tm
}

func pt(lat, lon float64) alert.Point { return alert.Point{Lat: lat, Lon: lon} }

func battery(pct int8) message.Message { return &ardupilotmega.MessageSysStatus{BatteryRemaining: pct} }

func at(lat, lon float64) message.Message {
	return &ardupilotmega.MessageGlobalPositionInt{Lat: int32(lat * 1e7), Lon: int32(lon * 1e7)}
}

func kinds(as []alert.Alert) []string {
	var out []string
	for _, a := range as {
		s := a.Kind + " cleared"
		if a.Raised {
			s = a.Kind + " raised"
		}
		out = append(out, s)
	}
	return out
}

func equal(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

func TestLowBatteryOnceWithHysteresis(t *testing.T) {
	s := &sink{}
	r := newRules(20, 3*time.Second, 10*time.Minute, s.emit)
	for i, pct := range []int8{50, 21, 20, 19, 22, 20, 24, 25, 30, -1} {
		r.observe(tel(t, time.Duration(i)*time.Second, battery(pct)))
	}
	if want := []string{"low_battery raised", "low_battery cleared"}; !equal(kinds(s.got), want) {
		t.Errorf("%v, want %v", kinds(s.got), want)
	}
	if s.got[0].Message != "Battery at 20%" || s.got[1].Message != "Battery back to 25%" {
		t.Errorf("%q, %q", s.got[0].Message, s.got[1].Message)
	}
}

func TestLinkLostAndRestored(t *testing.T) {
	s := &sink{}
	r := newRules(20, 3*time.Second, 10*time.Minute, s.emit)
	r.observe(tel(t, 0, &ardupilotmega.MessageHeartbeat{}))
	r.tick(t0.Add(2 * time.Second))
	if len(s.got) != 0 {
		t.Fatalf("early: %v", kinds(s.got))
	}
	r.tick(t0.Add(4 * time.Second))
	r.tick(t0.Add(5 * time.Second)) // still lost: no second alert
	r.observe(tel(t, 6*time.Second, &ardupilotmega.MessageHeartbeat{}))
	if want := []string{"link_lost raised", "link_lost cleared"}; !equal(kinds(s.got), want) {
		t.Errorf("%v, want %v", kinds(s.got), want)
	}
	if len(r.active()) != 0 {
		t.Errorf("active %v", r.active())
	}
}

func TestSilentDronesAreForgotten(t *testing.T) {
	s := &sink{}
	r := newRules(20, 3*time.Second, time.Minute, s.emit)
	r.observe(tel(t, 0, &ardupilotmega.MessageHeartbeat{}))
	r.tick(t0.Add(10 * time.Second))
	if len(r.active()) != 1 {
		t.Fatalf("active %v", r.active())
	}
	r.tick(t0.Add(2 * time.Minute))
	if len(r.active()) != 0 {
		t.Errorf("a drone gone for good still has alerts: %v", r.active())
	}
}

func TestGeofence(t *testing.T) {
	s := &sink{}
	r := newRules(20, 3*time.Second, 10*time.Minute, s.emit)
	fence := []alert.Point{pt(-35.37, 149.16), pt(-35.36, 149.16), pt(-35.36, 149.17), pt(-35.37, 149.17)}
	r.observe(tel(t, 0, at(-35.365, 149.165)))
	if len(s.got) != 0 {
		t.Fatalf("no fence yet, but %v", kinds(s.got))
	}
	r.setFence(fence, t0)
	if len(s.got) != 0 {
		t.Fatalf("inside, but %v", kinds(s.got))
	}
	r.observe(tel(t, time.Second, at(-35.355, 149.165)))
	r.observe(tel(t, 2*time.Second, at(-35.354, 149.165))) // still outside
	r.observe(tel(t, 3*time.Second, at(0, 0)))             // no fix: ignored
	r.observe(tel(t, 4*time.Second, at(-35.365, 149.165)))
	r.observe(tel(t, 5*time.Second, at(-35.355, 149.165)))
	r.setFence(nil, t0.Add(6*time.Second)) // removed: nothing to breach
	want := []string{"geofence raised", "geofence cleared", "geofence raised", "geofence cleared"}
	if !equal(kinds(s.got), want) {
		t.Errorf("%v, want %v", kinds(s.got), want)
	}
}

func TestNewFenceChecksEveryDroneAtOnce(t *testing.T) {
	s := &sink{}
	r := newRules(20, 3*time.Second, 10*time.Minute, s.emit)
	r.observe(tel(t, 0, at(-35.365, 149.165)))
	r.setFence([]alert.Point{pt(0, 0), pt(0, 1), pt(1, 1)}, t0)
	if want := []string{"geofence raised"}; !equal(kinds(s.got), want) {
		t.Errorf("%v, want %v", kinds(s.got), want)
	}
}
