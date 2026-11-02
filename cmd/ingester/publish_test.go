package main

import (
	"context"
	"io"
	"net/netip"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/bus/natstest"
	"github.com/ChengCK18/murmur/internal/linkstats"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/tlog"
)

type fakePublisher struct{ msgs []schema.Telemetry }

func (f *fakePublisher) publish(t schema.Telemetry) { f.msgs = append(f.msgs, t) }
func (f *fakePublisher) stats() (uint64, uint64)    { return uint64(len(f.msgs)), 0 }

// feed sends every record of a recording to in as packets from one address.
func feed(t *testing.T, in *ingester, name string) []tlog.Record {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	for _, rec := range recs {
		in.handlePacket(rec.Time, portA, rec.Frame)
	}
	return recs
}

// wantPublished counts the drone frames of a recording that should be published.
func wantPublished(t *testing.T, recs []tlog.Record) int {
	t.Helper()
	d, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, rec := range recs {
		frames, _ := d.Decode(rec.Time, netip.AddrPort{}, rec.Frame)
		for _, r := range frames {
			if r.SysID == 1 && published(r.Msg) {
				n++
			}
		}
	}
	return n
}

func TestPublishedMessages(t *testing.T) {
	in, err := newIngester(linkstats.DefaultTimeout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	pub := &fakePublisher{}
	in.pub, in.tenant = pub, 7
	recs := feed(t, in, "flight.tlog")

	if want := wantPublished(t, recs); len(pub.msgs) != want {
		t.Errorf("published %d, want %d", len(pub.msgs), want)
	}
	types := map[string]int{}
	ids := map[string]bool{}
	for _, m := range pub.msgs {
		types[m.Msg]++
		if m.TenantID != 7 || m.DroneID != 1 || m.SysID != 1 || m.Subject() != "telemetry.1" {
			t.Fatalf("message %+v", m)
		}
		if ids[m.DedupID()] {
			t.Fatalf("dedup ID %s used twice", m.DedupID())
		}
		ids[m.DedupID()] = true
	}
	// The key messages, counted in internal/mavlink's tests; nothing from the ground station.
	for k, n := range map[string]int{"HEARTBEAT": 165, "GLOBAL_POSITION_INT": 793, "ATTITUDE": 793, "SYS_STATUS": 317, "BATTERY_STATUS": 158} {
		if types[k] != n {
			t.Errorf("%s: %d, want %d", k, types[k], n)
		}
	}
	t.Logf("published by type: %v", types)
}

func TestPublishToJetStream(t *testing.T) {
	url := natstest.Start(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	p, err := newNATSPublisher(ctx, url, 256)
	if err != nil {
		t.Fatal(err)
	}
	defer p.close(5 * time.Second)

	stored := func() uint64 {
		t.Helper()
		select {
		case <-p.js.PublishAsyncComplete():
		case <-ctx.Done():
			t.Fatal("acknowledgements not received")
		}
		s, err := p.js.Stream(ctx, bus.TelemetryStream)
		if err != nil {
			t.Fatal(err)
		}
		info, err := s.Info(ctx)
		if err != nil {
			t.Fatal(err)
		}
		return info.State.Msgs
	}

	in, err := newIngester(linkstats.DefaultTimeout, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	in.pub = p
	recs := feed(t, in, "flight.tlog")
	want := uint64(wantPublished(t, recs))
	if n := stored(); n != want {
		t.Errorf("stream holds %d messages, want %d", n, want)
	}

	// The same flight again, as if every publish were retried: the dedup IDs
	// match, so JetStream stores nothing new.
	again, _ := newIngester(linkstats.DefaultTimeout, io.Discard)
	again.pub = p
	feed(t, again, "flight.tlog")
	if n := stored(); n != want {
		t.Errorf("after replaying the flight again the stream holds %d messages, want %d", n, want)
	}
	if published, errs := p.stats(); published != 2*want || errs != 0 {
		t.Errorf("published %d errors %d, want %d 0", published, errs, 2*want)
	}

	// A message reads back intact.
	s, _ := p.js.Stream(ctx, bus.TelemetryStream)
	msg, err := s.GetMsg(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if msg.Subject != "telemetry.1" || msg.Header.Get(jetstream.MsgIDHeader) == "" {
		t.Errorf("first message: subject %s header %v", msg.Subject, msg.Header)
	}
}
