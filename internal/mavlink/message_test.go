package mavlink

import "testing"

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
