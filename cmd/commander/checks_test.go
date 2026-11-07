package main

import (
	"context"
	"testing"

	"github.com/ChengCK18/murmur/internal/command"
	"github.com/ChengCK18/murmur/internal/state"
)

func TestPreconditions(t *testing.T) {
	air := &state.Position{RelAlt: 20}
	ground := &state.Position{RelAlt: 0.1}
	armed := func(mode string, p *state.Position) *state.Drone {
		return &state.Drone{Armed: true, Mode: mode, Position: p}
	}
	disarmed := &state.Drone{Mode: "GUIDED", Position: ground}

	cases := []struct {
		typ string
		d   *state.Drone
		ok  bool
	}{
		{command.Disarm, armed("GUIDED", ground), true},
		{command.Disarm, armed("GUIDED", air), false},
		{command.Takeoff, armed("GUIDED", ground), true},
		{command.Takeoff, armed("STABILIZE", ground), false},
		{command.Takeoff, disarmed, false},
		{command.ReturnHome, armed("GUIDED", air), true},
		{command.ReturnHome, armed("GUIDED", ground), false},
		{command.ReturnHome, disarmed, false},
		{command.Land, armed("AUTO", air), true},
		{command.Land, armed("AUTO", ground), false},
		{command.Land, armed("AUTO", nil), false}, // no position yet
		{command.MissionStart, armed("AUTO", ground), true},
		{command.MissionStart, disarmed, false},
		{command.Arm, disarmed, true},
		{command.SetMode, disarmed, true},
	}
	for _, c := range cases {
		if got := precondition(c.typ, c.d); (got == "") != c.ok {
			t.Errorf("%s on %+v: %q", c.typ, c.d, got)
		}
	}
}

func TestRefusedByPreconditionIsRecordedAndNeverSent(t *testing.T) {
	h := newHarness(t, fast)
	h.s.check = func(context.Context, command.Request) string {
		return "Takeoff needs the drone armed and in GUIDED mode"
	}
	u := h.s.submit(context.Background(), command.Request{DroneID: 42, Type: command.Takeoff, Params: command.Params{AltitudeM: 10}, IssuedBy: "demo"})
	if u.Status != command.Rejected || u.Reason == "" || u.CommandID == "" {
		t.Errorf("%+v", u)
	}
	if h.rows[u.CommandID] != command.Rejected || len(h.sends()) != 0 {
		t.Errorf("recorded %q, %d sends", h.rows[u.CommandID], len(h.sends()))
	}
	if len(h.statuses) != 1 {
		t.Errorf("other browsers were told %d times", len(h.statuses))
	}
}
