package command

import (
	"errors"
	"testing"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
)

func TestLong(t *testing.T) {
	cases := []struct {
		name   string
		c      Command
		cmd    common.MAV_CMD
		p1, p2 float32
		p7     float32
	}{
		{name: "arm", c: Command{Type: Arm}, cmd: common.MAV_CMD_COMPONENT_ARM_DISARM, p1: 1},
		{name: "disarm", c: Command{Type: Disarm}, cmd: common.MAV_CMD_COMPONENT_ARM_DISARM},
		{name: "guided", c: Command{Type: SetMode, Params: Params{Mode: "GUIDED"}}, cmd: common.MAV_CMD_DO_SET_MODE, p1: 1, p2: 4},
		{name: "takeoff", c: Command{Type: Takeoff, Params: Params{AltitudeM: 20}}, cmd: common.MAV_CMD_NAV_TAKEOFF, p7: 20},
		{name: "rtl", c: Command{Type: ReturnHome}, cmd: common.MAV_CMD_NAV_RETURN_TO_LAUNCH},
		{name: "land", c: Command{Type: Land}, cmd: common.MAV_CMD_NAV_LAND},
		{name: "start", c: Command{Type: MissionStart}, cmd: common.MAV_CMD_MISSION_START},
	}
	for _, c := range cases {
		t.Run(c.name, func(t *testing.T) {
			m, err := c.c.Long(7, 1)
			if err != nil {
				t.Fatal(err)
			}
			if m.TargetSystem != 7 || m.TargetComponent != 1 || m.Command != c.cmd {
				t.Errorf("addressed wrongly: %+v", m)
			}
			if m.Param1 != c.p1 || m.Param2 != c.p2 || m.Param7 != c.p7 {
				t.Errorf("params %v, %v, %v, want %v, %v, %v", m.Param1, m.Param2, m.Param7, c.p1, c.p2, c.p7)
			}
		})
	}
}

func TestConfirmationCountsResends(t *testing.T) {
	for attempt, want := range map[int]uint8{1: 0, 2: 1, 3: 2} {
		m, err := Command{Type: Arm, Attempt: attempt}.Long(1, 1)
		if err != nil || m.Confirmation != want {
			t.Errorf("attempt %d: confirmation %d, err %v, want %d", attempt, m.Confirmation, err, want)
		}
	}
}

func TestValidate(t *testing.T) {
	if err := Validate("self_destruct", Params{}); !errors.Is(err, ErrUnknownType) {
		t.Errorf("unknown type: %v", err)
	}
	if Validate(SetMode, Params{Mode: "SPORT"}) == nil {
		t.Error("a mode outside the plan's table was accepted")
	}
	if Validate(Takeoff, Params{}) == nil {
		t.Error("takeoff without an altitude was accepted")
	}
	if err := Validate(Land, Params{}); err != nil {
		t.Error(err)
	}
}

func TestFinal(t *testing.T) {
	for _, s := range []string{Pending, Sent} {
		if Final(s) {
			t.Errorf("%s counted as final", s)
		}
	}
	for _, s := range []string{Accepted, Rejected, Failed, TimedOut, Expired, Unreachable, Unknown} {
		if !Final(s) {
			t.Errorf("%s not counted as final", s)
		}
	}
}
