package main

import (
	"bytes"
	"context"
	"net/netip"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/ChengCK18/murmur/internal/linkstats"
	"github.com/ChengCK18/murmur/internal/registry"
	"github.com/ChengCK18/murmur/internal/tlog"
)

var (
	portA = netip.MustParseAddrPort("127.0.0.1:42318")
	portB = netip.MustParseAddrPort("127.0.0.1:52299")
)

// replay feeds a recording to an ingester as UDP packets would arrive, ticking
// every 100 ms of recorded time. Frames after the first silence longer than
// the link timeout come from portB instead of portA, as a restarted SITL does
// in mode C. It returns the final report and the event lines without their
// timestamps.
func replay(t *testing.T, name string) (report string, events []string) {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	in, err := newIngester(linkstats.DefaultTimeout, &out)
	if err != nil {
		t.Fatal(err)
	}
	src, last := portA, recs[0].Time
	next := recs[0].Time
	for _, rec := range recs {
		for ; !next.After(rec.Time); next = next.Add(100 * time.Millisecond) {
			in.tick(next)
		}
		if rec.Time.Sub(last) > linkstats.DefaultTimeout {
			src = portB
		}
		if rec.Frame[5] == 1 { // system ID: only the drone's silence counts
			last = rec.Time
		}
		in.handlePacket(rec.Time, src, rec.Frame)
	}
	report = in.tick(recs[len(recs)-1].Time)
	stamp := regexp.MustCompile(`^\d\d:\d\d:\d\d\.\d{3}  `)
	for _, l := range strings.Split(strings.TrimSpace(out.String()), "\n") {
		events = append(events, stamp.ReplaceAllString(l, ""))
	}
	return report, events
}

func checkReport(t *testing.T, report, header, drone string) {
	t.Helper()
	lines := strings.Split(strings.TrimSpace(report), "\n")
	if len(lines) != 2 {
		t.Fatalf("report has %d lines, want 2:\n%s", len(lines), report)
	}
	if !strings.HasSuffix(lines[0], header) {
		t.Errorf("header %q\nwant suffix %q", lines[0], header)
	}
	if lines[1] != drone {
		t.Errorf("drone line\n got %q\nwant %q", lines[1], drone)
	}
}

func checkEvents(t *testing.T, got, want []string) {
	t.Helper()
	if strings.Join(got, "\n") != strings.Join(want, "\n") {
		t.Errorf("events\n got %q\nwant %q", got, want)
	}
}

func TestReplayFlight(t *testing.T) {
	report, events := replay(t, "flight.tlog")
	// Final state cross-checked with pymavlink: landed at home after the
	// mission, still in AUTO, 72% battery.
	checkReport(t, report,
		"drones 1  packets 8180  decode errors 0  ignored 168  unregistered 0",
		"drone 1  sys 1  127.0.0.1:42318  AUTO disarmed  -35.36326,149.16524  alt 0.0 m  hdg 180°  batt 72%  lost 0  link OK")
	checkEvents(t, events, []string{
		"drone 1: new, sys 1 from 127.0.0.1:42318",
		"drone 1: link up",
	})
}

func TestReplayRestart(t *testing.T) {
	// The Phase 1 check, from a recording: the same drone ID after a restart,
	// link lost then restored, and no loss counted for the sequence reset.
	report, events := replay(t, "restart.tlog")
	checkReport(t, report,
		"drones 1  packets 10122  decode errors 0  ignored 213  unregistered 0",
		"drone 1  sys 1  127.0.0.1:52299  RTL disarmed  -35.36326,149.16524  alt 0.0 m  hdg 90°  batt 80%  lost 0  link OK")
	checkEvents(t, events, []string{
		"drone 1: new, sys 1 from 127.0.0.1:42318",
		"drone 1: link up",
		"drone 1: link lost",
		"drone 1: rebound from 127.0.0.1:42318 to 127.0.0.1:52299 (restarted)",
		"drone 1: link restored",
	})
}

func TestBootTextAndLinkLost(t *testing.T) {
	recs, err := tlog.ReadFile("../../testdata/idle.tlog")
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	in, err := newIngester(linkstats.DefaultTimeout, &out)
	if err != nil {
		t.Fatal(err)
	}
	at := recs[0].Time
	in.handlePacket(at, portA, []byte("\n\nInit ArduCopter V4.8.0-dev (9165d224)\n\nFree RAM: 524288\n"))
	in.handlePacket(at, portA, recs[0].Frame) // the drone's first HEARTBEAT

	report := in.tick(at.Add(5 * time.Second)) // no heartbeat since: link lost
	checkReport(t, report,
		"drones 1  packets 2  decode errors 1  ignored 0  unregistered 0",
		"drone 1  sys 1  127.0.0.1:42318  STABILIZE disarmed  no position  batt ?  lost 0  link LOST")
	if !strings.Contains(out.String(), "drone 1: link lost") {
		t.Errorf("no link lost event in:\n%s", out.String())
	}
}

// memStore is a registry.Store holding drones from an earlier ingester run.
type memStore struct{ drones []registry.Drone }

func (m *memStore) Load(context.Context) ([]registry.Drone, error) { return m.drones, nil }
func (m *memStore) Create(_ context.Context, d registry.Drone) (registry.ID, error) {
	return registry.ID(len(m.drones) + 1), nil
}
func (m *memStore) Move(context.Context, registry.Drone) error { return nil }

func TestDronesFromStore(t *testing.T) {
	// After an ingester restart the registry already knows drone 1, so its
	// first frame here is Known, or Rebound if SITL restarted meanwhile.
	recs, err := tlog.ReadFile("../../testdata/flight.tlog")
	if err != nil {
		t.Fatal(err)
	}
	earlier := recs[0].Time.Add(-time.Minute)
	for _, tt := range []struct {
		name  string
		src   netip.AddrPort
		event string
	}{
		{"same address", portA, "drone 1: known, sys 1 from 127.0.0.1:42318"},
		{"new address", portB, "drone 1: rebound from 127.0.0.1:42318 to 127.0.0.1:52299 (restarted)"},
	} {
		t.Run(tt.name, func(t *testing.T) {
			store := &memStore{drones: []registry.Drone{{ID: 1, Src: portA, SysID: 1, FirstSeen: earlier, LastHeartbeat: earlier}}}
			var out bytes.Buffer
			in, err := newIngester(linkstats.DefaultTimeout, &out)
			if err != nil {
				t.Fatal(err)
			}
			if in.reg, err = registry.NewWithStore(context.Background(), linkstats.DefaultTimeout, store); err != nil {
				t.Fatal(err)
			}
			for _, rec := range recs[:200] {
				in.handlePacket(rec.Time, tt.src, rec.Frame)
			}
			report := in.tick(recs[199].Time)
			if !strings.Contains(out.String(), tt.event) || !strings.Contains(report, "drone 1  sys 1  "+tt.src.String()) {
				t.Errorf("events:\n%s\nreport:\n%s", out.String(), report)
			}
		})
	}
}
