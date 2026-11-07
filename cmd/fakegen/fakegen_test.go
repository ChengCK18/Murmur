package main

import (
	"bytes"
	"context"
	"math"
	"net"
	"net/netip"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialect"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v3/pkg/frame"

	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
)

func TestRates(t *testing.T) {
	// One second is ten ticks: ten messages, whatever the route.
	for i := range 3 {
		p := newPath(i, 3)
		counts := map[string]int{}
		total := 0
		for n := range 10 {
			for _, m := range p.messages(n) {
				counts[schema.MessageName(m)]++
				total++
			}
		}
		want := map[string]int{"HEARTBEAT": 1, "GLOBAL_POSITION_INT": 5, "ATTITUDE": 2, "SYS_STATUS": 1, "BATTERY_STATUS": 1}
		for k, n := range want {
			if counts[k] != n {
				t.Errorf("drone %d %s: %d per second, want %d", i, k, counts[k], n)
			}
		}
		if total != 10 {
			t.Errorf("drone %d: %d messages per second, want 10", i, total)
		}
		if first := p.messages(0)[0]; !mavlink.IsHeartbeat(first) {
			t.Errorf("drone %d: first message is %T, want HEARTBEAT", i, first)
		}
	}
}

func TestRoutes(t *testing.T) {
	// Every route keeps a steady speed, moves without jumps, stays inside
	// its grid cell and heads the way it moves.
	const step = 100 * time.Millisecond
	kinds := map[string]bool{}
	for i := range 30 {
		p := newPath(i, 30)
		switch r := p.route.(type) {
		case circle:
			kinds["circle"] = true
		case loop:
			kinds[map[bool]string{true: "line", false: "lawnmower"}[len(r.points) == 2]] = true
		}
		cosLat := math.Cos(p.lat * math.Pi / 180)
		first := p.route.at(0)
		speed := math.Hypot(first.vn, first.ve)
		prev := first
		for n := 1; n <= 3000; n++ { // five minutes
			l := p.route.at(time.Duration(n) * step)
			if v := math.Hypot(l.vn, l.ve); math.Abs(v-speed) > 1e-9 {
				t.Fatalf("drone %d t=%v: speed %.3f, was %.3f", i, time.Duration(n)*step, v, speed)
			}
			if d := math.Hypot(l.north-prev.north, l.east-prev.east); d > speed*step.Seconds()+1e-6 {
				t.Fatalf("drone %d t=%v: jumped %.2f m", i, time.Duration(n)*step, d)
			}
			if math.Abs(l.north) > gridSpacing/2 || math.Abs(l.east) > gridSpacing/2 {
				t.Fatalf("drone %d t=%v: left its cell at %.0f m N, %.0f m E", i, time.Duration(n)*step, l.north, l.east)
			}
			prev = l
		}
		s := p.at(42 * time.Second)
		if want := math.Mod(math.Atan2(s.ve, s.vn)*180/math.Pi+360, 360); math.Abs(s.headingDeg-want) > 1e-9 {
			t.Errorf("drone %d: heading %.2f, want %.2f", i, s.headingDeg, want)
		}
		l := p.route.at(42 * time.Second)
		if north := (s.lat - p.lat) * metresPerDegLat; math.Abs(north-l.north) > 1e-6 {
			t.Errorf("drone %d: %.3f m north, route says %.3f", i, north, l.north)
		}
		if east := (s.lng - p.lng) * metresPerDegLat * cosLat; math.Abs(east-l.east) > 1e-6 {
			t.Errorf("drone %d: %.3f m east, route says %.3f", i, east, l.east)
		}
	}
	if len(kinds) != 3 {
		t.Errorf("routes flown: %v, want circle, line and lawnmower", kinds)
	}
	if a, b := newPath(0, 10), newPath(1, 10); a.lat == b.lat && a.lng == b.lng {
		t.Error("two drones share a cell")
	}
}

func TestCircle(t *testing.T) {
	c := circle{radius: 70, period: time.Minute, phase: 1}
	for _, sec := range []float64{0, 3.3, 15, 31, 59} {
		l := c.at(time.Duration(sec * float64(time.Second)))
		if r := math.Hypot(l.north, l.east); math.Abs(r-70) > 1e-9 {
			t.Errorf("t=%vs: %.2f m from the centre, want 70", sec, r)
		}
		if dot := l.north*l.vn + l.east*l.ve; math.Abs(dot) > 1e-6 {
			t.Errorf("t=%vs: velocity not tangent (dot %v)", sec, dot)
		}
	}
	// Anticlockwise: turning left, so banked left.
	s := path{route: c}.at(0)
	if s.yawRate >= 0 || s.bank >= 0 {
		t.Errorf("yaw rate %v, bank %v: want both negative (left turn)", s.yawRate, s.bank)
	}
}

func TestLawnmower(t *testing.T) {
	// Six rows 30 m apart: every row is flown in full, alternating direction,
	// and one lap returns to the start.
	l := newLawnmower(200, 6, 30, 8, 0)
	lap := time.Duration(l.length() / 8 * float64(time.Second))
	if want := 6*200 + 5*30 + 150.0; math.Abs(l.length()-want) > 1e-9 {
		t.Errorf("lap %.0f m, want %.0f", l.length(), want)
	}
	start, end := l.at(0), l.at(lap)
	if math.Hypot(end.north-start.north, end.east-start.east) > 1e-6 {
		t.Errorf("a lap ends at %+v, started at %+v", end, start)
	}
	rows := map[float64]float64{} // row north → direction east
	for d := 0.0; d < l.length(); d += 1 {
		p := l.at(time.Duration(d / 8 * float64(time.Second)))
		if p.vn == 0 {
			rows[math.Round(p.north)] = math.Copysign(1, p.ve)
		}
	}
	if len(rows) != 6 {
		t.Fatalf("rows flown %v, want 6", rows)
	}
	for _, n := range []float64{-75, -45, -15, 15, 45, 75} {
		if want := map[bool]float64{true: 1, false: -1}[int((n+75)/30)%2 == 0]; rows[n] != want {
			t.Errorf("row %.0f m: direction %v, want %v", n, rows[n], want)
		}
	}
}

func TestLine(t *testing.T) {
	// Back and forth: the far end is reached after one length.
	l := newLine(200, math.Pi/2, 10, 0) // west to east
	if a, b := l.at(0), l.at(20*time.Second); a.east != -100 || math.Abs(b.east-100) > 1e-9 || a.ve != 10 {
		t.Errorf("start %+v, after 20 s %+v", a, b)
	}
	if back := l.at(25 * time.Second); back.ve != -10 || math.Abs(back.east-50) > 1e-9 {
		t.Errorf("coming back %+v", back)
	}
}

func TestBattery(t *testing.T) {
	full, _, _ := battery(0)
	later, _, used := battery(10 * time.Minute)
	empty, _, _ := battery(5 * time.Hour)
	if full != 100 || later != 80 || empty != 5 || used != 2400 {
		t.Errorf("battery %d%% → %d%% (%d mAh) → %d%%", full, later, used, empty)
	}
}

func TestSend(t *testing.T) {
	// Exactly -messages frames arrive, from one source port per drone, each
	// sender's sequence unbroken and starting with a heartbeat.
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer ln.Close()

	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const drones, total = 3, 90
	done := make(chan result, 1)
	go func() {
		res, err := run(ctx, config{count: drones, target: ln.LocalAddr().String(), messages: total})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	d, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	type sender struct {
		frames  int
		lastSeq uint8
		sysID   uint8
	}
	senders := map[netip.AddrPort]*sender{}
	buf := make([]byte, 2048)
	got := 0
	for got < total {
		ln.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		n, src, err := ln.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatalf("after %d frames: %v", got, err)
		}
		frames, err := d.Decode(time.Now(), src, buf[:n])
		if err != nil || len(frames) != 1 {
			t.Fatalf("datagram %d: %d frames, err %v", got, len(frames), err)
		}
		r := frames[0]
		s, ok := senders[src]
		if !ok {
			if _, isHB := r.Msg.(*ardupilotmega.MessageHeartbeat); !isHB {
				t.Errorf("%v: first message %T, want HEARTBEAT", src, r.Msg)
			}
			s = &sender{sysID: r.SysID}
			senders[src] = s
		} else if r.Seq != s.lastSeq+1 {
			t.Errorf("%v: seq %d after %d", src, r.Seq, s.lastSeq)
		}
		s.frames++
		s.lastSeq = r.Seq
		got++
	}
	res := <-done
	if res.sent != total || res.errors != 0 {
		t.Errorf("result %+v, want %d sent", res, total)
	}
	// Nothing beyond the budget.
	ln.SetReadDeadline(time.Now().Add(300 * time.Millisecond)) //nolint:errcheck
	if _, _, err := ln.ReadFromUDPAddrPort(buf); err == nil {
		t.Error("more frames than -messages")
	}
	if len(senders) != drones {
		t.Fatalf("%d source ports, want %d", len(senders), drones)
	}
	ids := map[uint8]bool{}
	for _, s := range senders {
		ids[s.sysID] = true
	}
	if len(ids) != drones {
		t.Errorf("system IDs %v, want %d distinct", ids, drones)
	}
}

func listen(t *testing.T) *net.UDPConn {
	t.Helper()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close() })
	return ln
}

func TestDrop(t *testing.T) {
	// Dropped frames take a sequence number and never leave, and the count
	// fakegen reports is the count missing at the far end.
	ln := listen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	const total = 40
	done := make(chan result, 1)
	go func() {
		res, err := run(ctx, config{count: 1, target: ln.LocalAddr().String(), messages: total, drop: 0.25})
		if err != nil {
			t.Error(err)
		}
		done <- res
	}()

	d, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	got, top := 0, 0
	buf := make([]byte, 2048)
	var res result
	for finished := false; !finished; {
		ln.SetReadDeadline(time.Now().Add(100 * time.Millisecond)) //nolint:errcheck
		n, src, err := ln.ReadFromUDPAddrPort(buf)
		if err != nil {
			select {
			case res = <-done:
				finished = true
			default:
			}
			continue
		}
		frames, err := d.Decode(time.Now(), src, buf[:n])
		if err != nil || len(frames) != 1 {
			t.Fatalf("datagram: %d frames, err %v", len(frames), err)
		}
		got++
		top = max(top, int(frames[0].Seq))
	}
	if res.sent != total || res.dropped == 0 || int64(got) != res.sent-res.dropped {
		t.Errorf("sent %d, dropped %d, arrived %d", res.sent, res.dropped, got)
	}
	if top < got {
		t.Errorf("highest sequence %d with %d frames arrived: dropped frames took no number", top, got)
	}
}

func TestFollowsTheTarget(t *testing.T) {
	// A restarted ingester on a new address gets the traffic, from the same
	// source port.
	first, second := listen(t), listen(t)
	var moved atomic.Bool
	lookup := func() (*net.UDPAddr, error) {
		if moved.Load() {
			return second.LocalAddr().(*net.UDPAddr), nil
		}
		return first.LocalAddr().(*net.UDPAddr), nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	go run(ctx, config{count: 1, lookup: lookup, refresh: 10 * time.Millisecond}) //nolint:errcheck

	buf := make([]byte, 2048)
	first.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	_, before, err := first.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}
	moved.Store(true)
	second.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	_, after, err := second.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatalf("nothing at the new address: %v", err)
	}
	if before.Port() != after.Port() {
		t.Errorf("source port changed from %d to %d", before.Port(), after.Port())
	}
}

func long(cmd common.MAV_CMD, p1, p2, p7 float32) *ardupilotmega.MessageCommandLong {
	return &ardupilotmega.MessageCommandLong{TargetSystem: 1, TargetComponent: 1, Command: cmd, Param1: p1, Param2: p2, Param7: p7}
}

func TestPilotLandsThenFliesAgain(t *testing.T) {
	pl := newPilot(newPath(0, 1))
	second := func(from, to int) state {
		var s state
		for i := from; i <= to; i++ {
			s = pl.at(time.Duration(i) * time.Second)
		}
		return s
	}
	if s := second(0, 5); !s.armed || s.mode != modeAuto || s.alt == 0 {
		t.Fatalf("starts as %+v", s)
	}
	refused := []struct {
		name string
		m    *ardupilotmega.MessageCommandLong
	}{
		{"disarm in the air", long(common.MAV_CMD_COMPONENT_ARM_DISARM, 0, 0, 0)},
		{"takeoff outside GUIDED", long(common.MAV_CMD_NAV_TAKEOFF, 0, 0, 20)},
		{"a mode outside the table", long(common.MAV_CMD_DO_SET_MODE, 1, 2, 0)},
		{"a command it does not know", long(common.MAV_CMD_DO_SET_SERVO, 0, 0, 0)},
	}
	for _, c := range refused {
		if r := pl.command(c.m); r == ardupilotmega.MAV_RESULT_ACCEPTED {
			t.Errorf("%s: accepted", c.name)
		}
	}

	if r := pl.command(long(common.MAV_CMD_NAV_LAND, 0, 0, 0)); r != ardupilotmega.MAV_RESULT_ACCEPTED {
		t.Fatalf("land: %v", r)
	}
	start := pl.last
	s := second(6, 60)
	if s.alt != 0 || s.armed || s.mode != modeLand {
		t.Fatalf("after landing: %+v", s)
	}
	if s.lat != start.lat || s.lng != start.lng {
		t.Error("land moved sideways: it should descend where it is")
	}

	// Disarmed on the ground: the path back up is the one SITL needs.
	if r := pl.command(long(common.MAV_CMD_NAV_TAKEOFF, 0, 0, 20)); r != ardupilotmega.MAV_RESULT_DENIED {
		t.Errorf("takeoff while disarmed: %v", r)
	}
	for _, m := range []*ardupilotmega.MessageCommandLong{
		long(common.MAV_CMD_COMPONENT_ARM_DISARM, 1, 0, 0),
		long(common.MAV_CMD_DO_SET_MODE, 1, modeGuided, 0),
		long(common.MAV_CMD_NAV_TAKEOFF, 0, 0, 20),
		long(common.MAV_CMD_NAV_TAKEOFF, 0, 0, 20), // a resend
	} {
		if r := pl.command(m); r != ardupilotmega.MAV_RESULT_ACCEPTED {
			t.Fatalf("%v: %v", m.Command, r)
		}
	}
	s = second(61, 100)
	if s.alt != 20 || !s.armed || s.mode != modeGuided {
		t.Errorf("after takeoff: %+v", s)
	}
}

func TestPilotReturnsHomeAndLands(t *testing.T) {
	pl := newPilot(newPath(0, 1))
	pl.at(20 * time.Second)
	if r := pl.command(long(common.MAV_CMD_NAV_RETURN_TO_LAUNCH, 0, 0, 0)); r != ardupilotmega.MAV_RESULT_ACCEPTED {
		t.Fatalf("rtl: %v", r)
	}
	var s state
	for i := 21; i < 200; i++ {
		s = pl.at(time.Duration(i) * time.Second)
	}
	if s.alt != 0 || s.armed || s.mode != modeRTL || s.lat != pl.path.lat || s.lng != pl.path.lng {
		t.Errorf("after return: %+v, home %v %v", s, pl.path.lat, pl.path.lng)
	}
}

func TestAnswersCommands(t *testing.T) {
	ln := listen(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	go run(ctx, config{count: 1, target: ln.LocalAddr().String()}) //nolint:errcheck

	d, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	buf := make([]byte, 2048)
	ln.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
	_, drone, err := ln.ReadFromUDPAddrPort(buf)
	if err != nil {
		t.Fatal(err)
	}

	rw, err := dialect.NewReadWriter(ardupilotmega.Dialect)
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	w, err := frame.NewWriter(frame.WriterConf{Writer: &out, DialectRW: rw, OutVersion: frame.V2, OutSystemID: 255, OutComponentID: 190})
	if err != nil {
		t.Fatal(err)
	}
	if err := w.WriteMessage(long(common.MAV_CMD_NAV_LAND, 0, 0, 0)); err != nil {
		t.Fatal(err)
	}
	if _, err := ln.WriteToUDPAddrPort(out.Bytes(), drone); err != nil {
		t.Fatal(err)
	}

	for {
		ln.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		n, src, err := ln.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatalf("no answer: %v", err)
		}
		frames, _ := d.Decode(time.Now(), src, buf[:n])
		for _, f := range frames {
			ack, ok := f.Msg.(*ardupilotmega.MessageCommandAck)
			if !ok {
				continue
			}
			if ack.Command != common.MAV_CMD_NAV_LAND || ack.Result != ardupilotmega.MAV_RESULT_ACCEPTED ||
				ack.TargetSystem != 255 || ack.TargetComponent != 190 || src != drone {
				t.Errorf("ack %+v from %v", ack, src)
			}
			return
		}
	}
}

func TestRejectsAndDropsOnRequest(t *testing.T) {
	ask := func(t *testing.T, cfg config) (acked bool, r ardupilotmega.MAV_RESULT) {
		t.Helper()
		ln := listen(t)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		cfg.count, cfg.target = 1, ln.LocalAddr().String()
		go run(ctx, cfg) //nolint:errcheck
		d, _ := mavlink.NewDecoder()
		rw, _ := dialect.NewReadWriter(ardupilotmega.Dialect)
		buf := make([]byte, 2048)
		ln.SetReadDeadline(time.Now().Add(5 * time.Second)) //nolint:errcheck
		_, drone, err := ln.ReadFromUDPAddrPort(buf)
		if err != nil {
			t.Fatal(err)
		}
		var out bytes.Buffer
		w, _ := frame.NewWriter(frame.WriterConf{Writer: &out, DialectRW: rw, OutVersion: frame.V2, OutSystemID: 255, OutComponentID: 190})
		w.WriteMessage(long(common.MAV_CMD_NAV_LAND, 0, 0, 0)) //nolint:errcheck
		ln.WriteToUDPAddrPort(out.Bytes(), drone)              //nolint:errcheck
		until := time.Now().Add(700 * time.Millisecond)
		for time.Now().Before(until) {
			ln.SetReadDeadline(until) //nolint:errcheck
			n, src, err := ln.ReadFromUDPAddrPort(buf)
			if err != nil {
				break
			}
			frames, _ := d.Decode(time.Now(), src, buf[:n])
			for _, f := range frames {
				if ack, ok := f.Msg.(*ardupilotmega.MessageCommandAck); ok {
					return true, ack.Result
				}
			}
		}
		return false, 0
	}
	if acked, _ := ask(t, config{commandDrop: 1}); acked {
		t.Error("a dropped command was answered")
	}
	if acked, r := ask(t, config{commandReject: 1}); !acked || r != ardupilotmega.MAV_RESULT_TEMPORARILY_REJECTED {
		t.Errorf("reject: acked %v, result %v", acked, r)
	}
}

func TestPilotFliesAMission(t *testing.T) {
	pl := newPilot(newPath(0, 1))
	pl.at(time.Second)
	if r := pl.command(long(common.MAV_CMD_MISSION_START, 0, 0, 0)); r != ardupilotmega.MAV_RESULT_FAILED {
		t.Errorf("start with no mission: %v", r)
	}

	north := pl.path.lat + 0.0005
	items := []*ardupilotmega.MessageMissionItemInt{
		{Seq: 0, Command: common.MAV_CMD_NAV_WAYPOINT},
		{Seq: 1, Command: common.MAV_CMD_NAV_TAKEOFF, Z: 30},
		{Seq: 2, Command: common.MAV_CMD_NAV_WAYPOINT, X: int32(north * 1e7), Y: int32(pl.path.lng * 1e7), Z: 30},
		{Seq: 3, Command: common.MAV_CMD_NAV_RETURN_TO_LAUNCH},
	}
	if pl.missionCount(4) != 0 {
		t.Fatal("the upload should ask for item 0 first")
	}
	if next, done := pl.missionItem(items[2]); next != 0 || done {
		t.Errorf("out of turn: asks for %d, done %v", next, done)
	}
	for i, it := range items {
		pl.missionItem(it) // a repeat changes nothing
		next, done := pl.missionItem(it)
		if want := i == len(items)-1; done != want || (!want && next != i+1) {
			t.Errorf("item %d: next %d, done %v", i, next, done)
		}
	}
	if _, done := pl.missionItem(items[3]); !done {
		t.Error("a resent last item should be acknowledged again")
	}

	if r := pl.command(long(common.MAV_CMD_MISSION_START, 0, 0, 0)); r != ardupilotmega.MAV_RESULT_ACCEPTED {
		t.Fatalf("start: %v", r)
	}
	seen := false
	var s state
	for i := 2; i < 300; i++ {
		s = pl.at(time.Duration(i) * time.Second)
		if s.alt == 30 && math.Abs(s.lat-float64(int32(north*1e7))/1e7) < 1e-6 {
			seen = true
		}
	}
	if !seen {
		t.Error("never reached the waypoint at mission height")
	}
	if s.alt != 0 || s.armed || s.mode != modeRTL {
		t.Errorf("should end landed at home: %+v", s)
	}
}
