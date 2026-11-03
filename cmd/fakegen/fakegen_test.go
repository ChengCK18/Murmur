package main

import (
	"context"
	"math"
	"net"
	"net/netip"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
)

func TestRates(t *testing.T) {
	// One second is ten ticks.
	p := newPath(0, 1)
	counts := map[string]int{}
	for n := range 10 {
		for _, m := range p.messages(n) {
			counts[schema.MessageName(m)]++
		}
	}
	want := map[string]int{"HEARTBEAT": 1, "GLOBAL_POSITION_INT": 5, "ATTITUDE": 5, "SYS_STATUS": 2, "BATTERY_STATUS": 1}
	for k, n := range want {
		if counts[k] != n {
			t.Errorf("%s: %d per second, want %d", k, counts[k], n)
		}
	}
	if first := p.messages(0)[0]; !mavlink.IsHeartbeat(first) {
		t.Errorf("first message is %T, want HEARTBEAT", first)
	}
}

func TestPath(t *testing.T) {
	p := newPath(7, 10)
	cosLat := math.Cos(p.lat * math.Pi / 180)
	for _, sec := range []float64{0, 3.3, 15, 31, 59} {
		s := p.at(time.Duration(sec * float64(time.Second)))
		north := (s.lat - p.lat) * metresPerDegLat
		east := (s.lng - p.lng) * metresPerDegLat * cosLat
		if r := math.Hypot(north, east); math.Abs(r-p.radius) > 0.01 {
			t.Errorf("t=%vs: %.2f m from the centre, want %.0f", sec, r, p.radius)
		}
		// Velocity is along the circle, at a steady speed.
		if dot := north*s.vn + east*s.ve; math.Abs(dot) > 1e-6 {
			t.Errorf("t=%vs: velocity not tangent (dot %v)", sec, dot)
		}
		if v := math.Hypot(s.vn, s.ve); math.Abs(v-2*math.Pi*p.radius/60) > 1e-9 {
			t.Errorf("t=%vs: speed %.3f", sec, v)
		}
		if want := math.Mod(math.Atan2(s.ve, s.vn)*180/math.Pi+360, 360); math.Abs(s.headingDeg-want) > 1e-9 {
			t.Errorf("t=%vs: heading %.2f, want %.2f", sec, s.headingDeg, want)
		}
	}
	if a, b := newPath(0, 10), newPath(1, 10); a.lat == b.lat && a.lng == b.lng {
		t.Error("two drones share a centre")
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
