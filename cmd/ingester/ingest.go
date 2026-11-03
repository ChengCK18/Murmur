package main

import (
	"fmt"
	"io"
	"net/netip"
	"strings"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/linkstats"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/registry"
	"github.com/ChengCK18/murmur/internal/schema"
)

// telemetry is the latest state of one drone, for the console.
type telemetry struct {
	src   netip.AddrPort
	sysID uint8
	comps map[uint8]bool // component IDs seen, to find the drone's link senders

	mode    string // empty until the autopilot's first heartbeat
	armed   bool
	pos     *ardupilotmega.MessageGlobalPositionInt
	battery int8 // SYS_STATUS.battery_remaining; -1 when unknown
}

// ingester turns received packets into drone state. Packets come from the
// UDP read loop and reports from a ticker, so it is safe for concurrent use.
type ingester struct {
	dec    *mavlink.Decoder
	reg    *registry.Registry
	links  *linkstats.Tracker
	events io.Writer // one line per notable event
	pub    publisher // nil: console only
	tenant int64

	mu           sync.Mutex
	drones       map[registry.ID]*telemetry
	packets      uint64
	frames       uint64
	decodeErrors uint64
	ignored      uint64 // ground-station frames
	unregistered uint64 // frames from senders not yet seen sending a heartbeat
}

func newIngester(timeout time.Duration, events io.Writer) (*ingester, error) {
	dec, err := mavlink.NewDecoder()
	if err != nil {
		return nil, err
	}
	return &ingester{
		dec:    dec,
		reg:    registry.New(timeout),
		links:  linkstats.New(timeout),
		events: events,
		drones: map[registry.ID]*telemetry{},
	}, nil
}

// handlePacket processes one UDP datagram.
func (in *ingester) handlePacket(at time.Time, src netip.AddrPort, data []byte) {
	frames, err := in.dec.Decode(at, src, data)
	in.mu.Lock()
	in.packets++
	in.frames += uint64(len(frames))
	if err != nil {
		in.decodeErrors++ // SITL's boot text lands here (finding 3)
	}
	in.mu.Unlock()
	for _, r := range frames {
		in.handle(r)
	}
}

// handle processes one decoded frame and publishes it if it comes from a
// drone. Publishing happens outside in.mu, so a slow NATS cannot hold up the
// console report.
func (in *ingester) handle(r mavlink.Received) {
	id, ok := in.update(r)
	if !ok || in.pub == nil || !published(r.Msg) {
		return
	}
	t, err := schema.FromReceived(in.tenant, int64(id), r)
	if err != nil {
		return // not in the dialect: published() only lets known types through
	}
	in.pub.publish(t)
}

// update applies one frame to the registry, link stats and drone state, and
// returns the drone it came from.
func (in *ingester) update(r mavlink.Received) (registry.ID, bool) {
	res := in.reg.Resolve(r)

	in.mu.Lock()
	defer in.mu.Unlock()

	switch res.Outcome {
	case registry.Ignored:
		in.ignored++
		return 0, false
	case registry.Unregistered:
		in.unregistered++
		return 0, false
	}

	// Console state is made on a drone's first frame, however the registry
	// knows it: a drone loaded from the store is Known (or Rebound) without
	// ever having been Registered by this ingester.
	t, seen := in.drones[res.Drone.ID]
	if !seen {
		t = &telemetry{comps: map[uint8]bool{}, battery: -1}
		in.drones[res.Drone.ID] = t
		if res.Outcome == registry.Known {
			in.eventf(r.ReceivedAt, "drone %d: known, sys %d from %v", res.Drone.ID, r.SysID, r.Src)
		}
	}
	switch res.Outcome {
	case registry.Registered:
		in.eventf(r.ReceivedAt, "drone %d: new, sys %d from %v", res.Drone.ID, r.SysID, r.Src)
	case registry.Rebound:
		for comp := range t.comps {
			in.links.Forget(linkstats.Key{Src: res.OldSrc, SysID: r.SysID, CompID: comp})
		}
		clear(t.comps)
		in.eventf(r.ReceivedAt, "drone %d: rebound from %v to %v (restarted)", res.Drone.ID, res.OldSrc, r.Src)
	}

	t.src, t.sysID = res.Drone.Src, res.Drone.SysID
	t.comps[r.CompID] = true

	switch ev := in.links.Observe(r); ev {
	case linkstats.LinkUp, linkstats.LinkRestored:
		if res.Outcome == registry.Rebound {
			ev = linkstats.LinkRestored // a new sender to linkstats, but the same drone back
		}
		in.eventf(r.ReceivedAt, "drone %d: %v", res.Drone.ID, ev)
	}

	switch m := r.Msg.(type) {
	case *ardupilotmega.MessageHeartbeat:
		if m.Autopilot != ardupilotmega.MAV_AUTOPILOT_INVALID { // the flight controller, not a camera or gimbal
			t.mode, t.armed = mavlink.ModeName(m), mavlink.IsArmed(m)
		}
	case *ardupilotmega.MessageGlobalPositionInt:
		t.pos = m
	case *ardupilotmega.MessageSysStatus:
		t.battery = m.BatteryRemaining
	}
	return res.Drone.ID, true
}

// eventf writes one event line. The caller holds in.mu.
func (in *ingester) eventf(at time.Time, format string, args ...any) {
	fmt.Fprintf(in.events, "%s  "+format+"\n", append([]any{at.Format("15:04:05.000")}, args...)...)
}

// tick checks link timeouts at now and returns the console report: a summary
// line, then one line per drone.
func (in *ingester) tick(now time.Time) string {
	changes := in.links.Tick(now)

	in.mu.Lock()
	defer in.mu.Unlock()

	for _, c := range changes {
		if id, ok := in.droneFor(c.Key); ok {
			in.eventf(now, "drone %d: %v", id, c.Event)
		}
	}

	var b strings.Builder
	fmt.Fprintf(&b, "--- %s  drones %d  packets %d  decode errors %d  ignored %d  unregistered %d",
		now.Format("15:04:05"), len(in.drones), in.packets, in.decodeErrors, in.ignored, in.unregistered)
	if in.pub != nil {
		published, errs := in.pub.stats()
		fmt.Fprintf(&b, "  published %d  publish errors %d", published, errs)
	}
	b.WriteByte('\n')
	for _, d := range in.reg.Drones() {
		b.WriteString(in.line(d.ID, in.drones[d.ID]))
		b.WriteByte('\n')
	}
	return b.String()
}

// droneFor finds the drone a link sender belongs to. The caller holds in.mu.
func (in *ingester) droneFor(k linkstats.Key) (registry.ID, bool) {
	for id, t := range in.drones {
		if t.src == k.Src && t.sysID == k.SysID && t.comps[k.CompID] {
			return id, true
		}
	}
	return 0, false
}

// line formats one drone, as in guide step 1.5:
//
//	drone 1  sys 1  127.0.0.1:43694  GUIDED armed  -35.36326,149.16524  alt 20.1 m  hdg 353°  batt 87%  lost 0  link OK
//
// The caller holds in.mu.
func (in *ingester) line(id registry.ID, t *telemetry) string {
	mode := t.mode
	if mode == "" {
		mode = "?"
	}
	armed := "disarmed"
	if t.armed {
		armed = "armed"
	}
	pos := "no position"
	if p := t.pos; p != nil {
		hdg := "hdg ?" // UINT16_MAX: unknown
		if p.Hdg != 65535 {
			hdg = fmt.Sprintf("hdg %d°", (int(p.Hdg)+50)/100%360)
		}
		pos = fmt.Sprintf("%.5f,%.5f  alt %.1f m  %s", float64(p.Lat)/1e7, float64(p.Lon)/1e7, float64(p.RelativeAlt)/1000, hdg)
	}
	batt := "batt ?"
	if t.battery >= 0 {
		batt = fmt.Sprintf("batt %d%%", t.battery)
	}

	var lost uint64
	link := "LOST"
	for comp := range t.comps {
		s, ok := in.links.Get(linkstats.Key{Src: t.src, SysID: t.sysID, CompID: comp})
		if !ok {
			continue
		}
		lost += s.Lost
		if s.LinkUp {
			link = "OK"
		}
	}
	return fmt.Sprintf("drone %d  sys %d  %v  %s %s  %s  %s  lost %d  link %s",
		id, t.sysID, t.src, mode, armed, pos, batt, lost, link)
}
