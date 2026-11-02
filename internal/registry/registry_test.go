package registry

import (
	"net/netip"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/linkstats"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/tlog"
)

var t0 = time.Unix(1793516806, 0)

// The two SITL addresses seen while checking the environment (finding 4).
var (
	portA = netip.MustParseAddrPort("127.0.0.1:42318")
	portB = netip.MustParseAddrPort("127.0.0.1:52299")
	other = netip.MustParseAddrPort("10.0.0.7:42318")
)

func heartbeat() message.Message {
	return &ardupilotmega.MessageHeartbeat{Type: ardupilotmega.MAV_TYPE_QUADROTOR}
}
func gcsHeartbeat() message.Message {
	return &ardupilotmega.MessageHeartbeat{Type: ardupilotmega.MAV_TYPE_GCS}
}
func sysStatus() message.Message { return &ardupilotmega.MessageSysStatus{} }

func frame(src netip.AddrPort, sys, comp uint8, at time.Duration, msg message.Message) mavlink.Received {
	return mavlink.Received{ReceivedAt: t0.Add(at), Src: src, SysID: sys, CompID: comp, Msg: msg}
}

type step struct {
	f       mavlink.Received
	outcome Outcome
	id      ID // expected drone ID for Known, Registered and Rebound
}

func run(t *testing.T, g *Registry, steps []step) {
	t.Helper()
	for i, s := range steps {
		res := g.Resolve(s.f)
		if res.Outcome != s.outcome || (s.id != 0 && res.Drone.ID != s.id) {
			t.Errorf("step %d (%v sys %d): %v drone %d, want %v drone %d",
				i, s.f.Src, s.f.SysID, res.Outcome, res.Drone.ID, s.outcome, s.id)
		}
	}
}

func TestIDs(t *testing.T) {
	run(t, New(linkstats.DefaultTimeout), []step{
		{frame(portA, 1, 1, 0, sysStatus()), Unregistered, 0}, // stray packet: no drone yet
		{frame(portA, 1, 1, 0, heartbeat()), Registered, 1},
		{frame(portA, 1, 1, 0, sysStatus()), Known, 1},
		{frame(portA, 1, 1, time.Second, heartbeat()), Known, 1},
		{frame(portB, 2, 1, 0, sysStatus()), Unregistered, 0},
		{frame(portB, 2, 1, 0, heartbeat()), Registered, 2},
		{frame(portA, 1, 154, 0, sysStatus()), Known, 1},    // another component of drone 1 (a gimbal)
		{frame(portA, 3, 1, 0, heartbeat()), Registered, 3}, // a second system behind the same address
	})
}

func TestGroundStationIgnored(t *testing.T) {
	g := New(linkstats.DefaultTimeout)
	run(t, g, []step{
		{frame(portA, 255, 190, 0, gcsHeartbeat()), Ignored, 0}, // MAVProxy, QGC, record.py
		{frame(portA, 255, 0, 0, heartbeat()), Ignored, 0},      // system ID 255 alone
		{frame(portA, 7, 190, 0, heartbeat()), Ignored, 0},      // component ID 190 alone
		{frame(portA, 9, 1, 0, gcsHeartbeat()), Ignored, 0},     // a GCS using another ID
	})
	if n := len(g.Drones()); n != 0 {
		t.Errorf("%d drones registered from ground-station traffic", n)
	}
}

func TestRebind(t *testing.T) {
	g := New(linkstats.DefaultTimeout)
	run(t, g, []step{
		{frame(portA, 1, 1, 0, heartbeat()), Registered, 1},
		// SITL restarted 7 s later: same IP and system ID, new port.
		{frame(portB, 1, 1, 7*time.Second, sysStatus()), Unregistered, 0}, // waits for a heartbeat
	})
	res := g.Resolve(frame(portB, 1, 1, 7*time.Second, heartbeat()))
	if res.Outcome != Rebound || res.Drone.ID != 1 || res.Drone.Src != portB || res.OldSrc != portA || res.Drone.Rebinds != 1 {
		t.Fatalf("got %+v, want drone 1 rebound from %v to %v", res, portA, portB)
	}
	run(t, g, []step{
		{frame(portB, 1, 1, 7*time.Second, sysStatus()), Known, 1},
		{frame(portA, 1, 1, 8*time.Second, sysStatus()), Unregistered, 0}, // the old address no longer belongs to it
	})
	if d := g.Drones(); len(d) != 1 {
		t.Errorf("drones %+v, want only drone 1", d)
	}
}

func TestNoRebind(t *testing.T) {
	tests := []struct {
		name  string
		src   netip.AddrPort
		sysID uint8
		after time.Duration
	}{
		{"old drone still alive", portB, 1, 2 * time.Second}, // within the 3 s timeout
		{"different IP", other, 1, 7 * time.Second},
		{"different system ID", portB, 2, 7 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			run(t, New(linkstats.DefaultTimeout), []step{
				{frame(portA, 1, 1, 0, heartbeat()), Registered, 1},
				{frame(tt.src, tt.sysID, 1, tt.after, heartbeat()), Registered, 2},
			})
		})
	}
}

func TestRebindPicksLatest(t *testing.T) {
	// Two timed-out drones could match; the one heard from last wins.
	portC := netip.MustParseAddrPort("127.0.0.1:60001")
	g := New(linkstats.DefaultTimeout)
	run(t, g, []step{
		{frame(portA, 1, 1, 0, heartbeat()), Registered, 1},
		{frame(portB, 1, 1, time.Second, heartbeat()), Registered, 2}, // drone 1 not yet timed out
		{frame(portC, 1, 1, 10*time.Second, heartbeat()), Rebound, 2},
	})
}

func TestRestartRecording(t *testing.T) {
	// A .tlog has no addresses, so give the frames before the restart one port
	// and those after it another, as SITL does in mode C. The restart is the
	// only silence longer than the link timeout in testdata/restart.tlog.
	recs, err := tlog.ReadFile("../../testdata/restart.tlog")
	if err != nil {
		t.Fatal(err)
	}
	d, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	g := New(linkstats.DefaultTimeout)
	src := portA
	var lastDrone time.Time
	count := map[Outcome]int{}
	for _, rec := range recs {
		frames, err := d.Decode(rec.Time, netip.AddrPort{}, rec.Frame)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range frames {
			if r.SysID == 1 {
				if !lastDrone.IsZero() && rec.Time.Sub(lastDrone) > linkstats.DefaultTimeout {
					src = portB
				}
				lastDrone = rec.Time
			}
			r.Src = src
			res := g.Resolve(r)
			count[res.Outcome]++
			if res.Outcome == Rebound && (res.Drone.ID != 1 || res.OldSrc != portA) {
				t.Errorf("rebound %+v", res)
			}
		}
	}
	if count[Registered] != 1 || count[Rebound] != 1 || count[Unregistered] != 0 {
		t.Errorf("outcomes %v, want one new, one rebound, none unregistered", count)
	}
	drones := g.Drones()
	if len(drones) != 1 || drones[0].ID != 1 || drones[0].Src != portB {
		t.Errorf("drones %+v, want drone 1 at %v", drones, portB)
	}
}
