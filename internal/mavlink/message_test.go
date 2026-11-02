package mavlink

import (
	"fmt"
	"testing"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
)

func TestMessageHelpers(t *testing.T) {
	var heartbeats, timed int
	for _, r := range decodeFile(t, "flight.tlog") {
		if IsHeartbeat(r.Msg) {
			heartbeats++
			if _, ok := TimeBootMs(r.Msg); ok {
				t.Error("HEARTBEAT has no time_boot_ms")
			}
		}
		if _, ok := TimeBootMs(r.Msg); ok && r.SysID == 1 {
			timed++
		}
	}
	if heartbeats != 165+156 {
		t.Errorf("%d heartbeats, want %d", heartbeats, 165+156)
	}
	// ATTITUDE and GLOBAL_POSITION_INT alone give 793 each.
	if timed < 2*793 {
		t.Errorf("only %d drone frames with time_boot_ms", timed)
	}
}

func TestModeNameAndArmed(t *testing.T) {
	var seq []string
	for _, r := range decodeFile(t, "flight.tlog") {
		hb, ok := r.Msg.(*ardupilotmega.MessageHeartbeat)
		if !ok || r.SysID != 1 {
			continue
		}
		s := ModeName(hb)
		if IsArmed(hb) {
			s += " armed"
		}
		if len(seq) == 0 || seq[len(seq)-1] != s {
			seq = append(seq, s)
		}
	}
	want := "[STABILIZE GUIDED GUIDED armed AUTO armed AUTO]"
	if got := fmt.Sprint(seq); got != want {
		t.Errorf("modes %s, want %s", got, want)
	}
	if got := ModeName(&ardupilotmega.MessageHeartbeat{Autopilot: ardupilotmega.MAV_AUTOPILOT_PX4, CustomMode: 4}); got != "MODE 4" {
		t.Errorf("PX4 heartbeat: %q, want MODE 4", got)
	}
}
