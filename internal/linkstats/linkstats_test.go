package linkstats

import (
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/tlog"
)

var (
	t0    = time.Unix(1793516806, 0)
	drone = Key{Src: netip.MustParseAddrPort("127.0.0.1:42318"), SysID: 1, CompID: 1}
)

func frame(k Key, at time.Duration, seq uint8, msg message.Message) mavlink.Received {
	return mavlink.Received{ReceivedAt: t0.Add(at), Src: k.Src, SysID: k.SysID, CompID: k.CompID, Seq: seq, Msg: msg}
}

func heartbeat() message.Message         { return &ardupilotmega.MessageHeartbeat{} }
func sysStatus() message.Message         { return &ardupilotmega.MessageSysStatus{} }
func attitude(ms uint32) message.Message { return &ardupilotmega.MessageAttitude{TimeBootMs: ms} }

func TestSequenceGaps(t *testing.T) {
	ms := time.Millisecond
	tests := []struct {
		name         string
		last, seq    uint8
		after        time.Duration   // time between the two frames
		prev, next   message.Message // the two messages
		lost, resets uint64
	}{
		// The table from guide step 1.7.
		{"next in order", 10, 11, 200 * ms, sysStatus(), sysStatus(), 0, 0},
		{"wrap 255 to 0", 255, 0, 200 * ms, sysStatus(), sysStatus(), 0, 0},
		{"7 lost across the wrap", 250, 2, 200 * ms, sysStatus(), sysStatus(), 7, 0},
		{"backwards: reordering", 10, 9, 200 * ms, sysStatus(), sysStatus(), 0, 1},
		{"largest loss counted", 10, 138, 200 * ms, sysStatus(), sysStatus(), 127, 0},
		{"gap of 128 is a reset", 10, 139, 200 * ms, sysStatus(), sysStatus(), 0, 1},
		// The SITL restart in testdata/restart.tlog: 176 → 0 is a gap of 79.
		{"restart after silence", 176, 0, 7600 * ms, sysStatus(), heartbeat(), 0, 1},
		{"restart seen by boot clock", 176, 0, 500 * ms, attitude(74610), attitude(2607), 0, 1},
		{"boot clock within margin", 176, 177, 200 * ms, attitude(74610), attitude(74000), 0, 0},
		// Without either clue a fast restart looks like loss; accepted, as
		// SITL takes longer than the timeout to come back.
		{"fast restart, no clues", 176, 0, 500 * ms, sysStatus(), sysStatus(), 79, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			tr := New(DefaultTimeout)
			tr.Observe(frame(drone, 0, tt.last, tt.prev))
			tr.Observe(frame(drone, tt.after, tt.seq, tt.next))
			s, _ := tr.Get(drone)
			if s.Lost != tt.lost || s.Resets != tt.resets || s.Frames != 2 || s.LastSeq != tt.seq {
				t.Errorf("lost %d resets %d frames %d last %d; want lost %d resets %d frames 2 last %d",
					s.Lost, s.Resets, s.Frames, s.LastSeq, tt.lost, tt.resets, tt.seq)
			}
		})
	}
}

func TestSendersCountedSeparately(t *testing.T) {
	gcs := Key{Src: drone.Src, SysID: 255, CompID: 190}
	other := Key{Src: netip.MustParseAddrPort("127.0.0.1:52299"), SysID: 1, CompID: 1}
	tr := New(DefaultTimeout)
	// Interleaved: each sender's own sequence is unbroken.
	tr.Observe(frame(drone, 0, 0, heartbeat()))
	tr.Observe(frame(gcs, 0, 100, heartbeat()))
	tr.Observe(frame(other, 0, 50, heartbeat()))
	tr.Observe(frame(drone, time.Millisecond, 1, sysStatus()))
	tr.Observe(frame(gcs, time.Millisecond, 101, sysStatus()))
	tr.Observe(frame(other, time.Millisecond, 51, sysStatus()))
	for _, k := range []Key{drone, gcs, other} {
		if s, _ := tr.Get(k); s.Lost != 0 || s.Resets != 0 || s.Frames != 2 {
			t.Errorf("%v: %+v", k, s)
		}
	}
}

func TestLinkTimeout(t *testing.T) {
	tr := New(DefaultTimeout)
	sec := time.Second
	steps := []struct {
		at   time.Duration
		hb   bool // true: a heartbeat arrives; false: Tick
		want Event
	}{
		{0, true, LinkUp},
		{1 * sec, true, None},
		{3900 * time.Millisecond, false, None}, // 2.9 s since the last heartbeat
		{4100 * time.Millisecond, false, LinkLost},
		{5 * sec, false, None}, // reported once only
		{6 * sec, true, LinkRestored},
		{7 * sec, true, None},
	}
	seq := uint8(0)
	for _, st := range steps {
		var got Event
		if st.hb {
			got = tr.Observe(frame(drone, st.at, seq, heartbeat()))
			seq++
		} else if ch := tr.Tick(t0.Add(st.at)); len(ch) == 1 && ch[0].Key == drone {
			got = ch[0].Event
		} else if len(ch) > 1 {
			t.Fatalf("at %v: %d changes", st.at, len(ch))
		}
		if got != st.want {
			t.Errorf("at %v: %v, want %v", st.at, got, st.want)
		}
	}
	if s, _ := tr.Get(drone); !s.LinkUp || s.LinkLosses != 1 {
		t.Errorf("LinkUp %v LinkLosses %d, want true 1", s.LinkUp, s.LinkLosses)
	}
}

func TestNoLinkBeforeHeartbeat(t *testing.T) {
	tr := New(DefaultTimeout)
	if ev := tr.Observe(frame(drone, 0, 0, sysStatus())); ev != None {
		t.Errorf("non-heartbeat frame gave %v", ev)
	}
	if ch := tr.Tick(t0.Add(time.Minute)); len(ch) != 0 {
		t.Errorf("a link that was never up was reported lost: %v", ch)
	}
}

func TestForget(t *testing.T) {
	tr := New(DefaultTimeout)
	tr.Observe(frame(drone, 0, 0, heartbeat()))
	tr.Forget(drone)
	if _, ok := tr.Get(drone); ok {
		t.Error("sender still tracked after Forget")
	}
	if ch := tr.Tick(t0.Add(time.Minute)); len(ch) != 0 {
		t.Errorf("forgotten sender reported: %v", ch)
	}
}

// replay feeds a recording through a Tracker, ticking every 100 ms of recorded
// time as the ingester's ticker would, and returns the drone's link events.
func replay(t *testing.T, name string) (*Tracker, []string) {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	tr := New(DefaultTimeout)
	var events []string
	note := func(at time.Time, k Key, ev Event) {
		if k.SysID == 1 && ev != None {
			events = append(events, fmt.Sprintf("%.1fs %v", at.Sub(recs[0].Time).Seconds(), ev))
		}
	}
	next := recs[0].Time
	for _, rec := range recs {
		for ; !next.After(rec.Time); next = next.Add(100 * time.Millisecond) {
			for _, c := range tr.Tick(next) {
				note(next, c.Key, c.Event)
			}
		}
		frames, err := d.Decode(rec.Time, netip.AddrPort{}, rec.Frame)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range frames {
			note(rec.Time, Key{SysID: r.SysID, CompID: r.CompID}, tr.Observe(r))
		}
	}
	return tr, events
}

func TestReplayFlight(t *testing.T) {
	tr, events := replay(t, "flight.tlog")
	if len(events) != 1 || events[0] != "0.0s link up" {
		t.Errorf("events %v, want only link up at the start", events)
	}
	for _, k := range []Key{{SysID: 1, CompID: 1}, {SysID: 255, CompID: 190}} {
		if s, _ := tr.Get(k); s.Lost != 0 || s.Resets != 0 {
			t.Errorf("%d/%d: lost %d resets %d, want 0 0", k.SysID, k.CompID, s.Lost, s.Resets)
		}
	}
}

func TestReplayRestart(t *testing.T) {
	// SITL is killed mid-air at about 75 s, silent for 7.6 s, then restarts
	// with its sequence back at 0.
	tr, events := replay(t, "restart.tlog")
	if len(events) != 3 || events[0] != "0.0s link up" ||
		events[1][len(events[1])-9:] != "link lost" || events[2][len(events[2])-13:] != "link restored" {
		t.Fatalf("events %v, want link up, link lost, link restored", events)
	}
	s, _ := tr.Get(Key{SysID: 1, CompID: 1})
	if s.Lost != 0 || s.Resets != 1 || s.LinkLosses != 1 || !s.LinkUp {
		t.Errorf("lost %d resets %d losses %d up %v; want 0 1 1 true", s.Lost, s.Resets, s.LinkLosses, s.LinkUp)
	}
	t.Logf("events: %v", events)
}

func TestRestartCountedOnce(t *testing.T) {
	// After a restart found from silence, the new boot clock must not be
	// compared with the old one (which would count the restart twice).
	tr := New(DefaultTimeout)
	tr.Observe(frame(drone, 0, 175, attitude(74400)))
	tr.Observe(frame(drone, 200*time.Millisecond, 176, attitude(74610)))
	tr.Observe(frame(drone, 7800*time.Millisecond, 0, heartbeat()))
	tr.Observe(frame(drone, 7900*time.Millisecond, 1, attitude(2607)))
	if s, _ := tr.Get(drone); s.Resets != 1 || s.Lost != 0 {
		t.Errorf("resets %d lost %d, want 1 0", s.Resets, s.Lost)
	}
}

func TestTotals(t *testing.T) {
	other := Key{Src: netip.MustParseAddrPort("127.0.0.1:52299"), SysID: 2, CompID: 1}
	tr := New(DefaultTimeout)
	tr.Observe(frame(drone, 0, 10, heartbeat()))
	tr.Observe(frame(drone, 100*time.Millisecond, 14, sysStatus())) // 3 lost
	tr.Observe(frame(other, 0, 0, heartbeat()))
	tr.Observe(frame(other, 100*time.Millisecond, 200, sysStatus())) // a reset
	tr.Tick(t0.Add(3500 * time.Millisecond))                         // both links lost
	tr.Observe(frame(drone, 4*time.Second, 15, heartbeat()))         // drone's back
	got := tr.Totals()
	want := Totals{Frames: 5, Lost: 3, Resets: 2, LinkLosses: 2, LinkUp: 1, Senders: 2}
	if got != want {
		t.Errorf("totals %+v, want %+v", got, want)
	}
}
