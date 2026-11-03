package main

import (
	"context"
	"encoding/json"
	"math/rand/v2"
	"net/netip"
	"strconv"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/bus/natstest"
	"github.com/ChengCK18/murmur/internal/db"
	"github.com/ChengCK18/murmur/internal/db/dbtest"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/metrics"
	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/tlog"
)

// newRedis returns a client on an empty Redis database (1 to 15), flushed
// again afterwards, or skips the test if Redis is not running.
func newRedis(t *testing.T) *redis.Client {
	t.Helper()
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: 1 + rand.IntN(15)})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("no Redis at 127.0.0.1:6379 (start deploy/docker-compose.yml): %v", err)
	}
	rdb.FlushDB(ctx)
	t.Cleanup(func() { rdb.FlushDB(ctx); rdb.Close() })
	return rdb
}

func tel(t *testing.T, at time.Time, msg message.Message) schema.Telemetry {
	t.Helper()
	m, err := schema.FromReceived(1, 7, mavlink.Received{ReceivedAt: at, SysID: 1, CompID: 1, Msg: msg})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestTypedColumns(t *testing.T) {
	at := time.Unix(1793516806, 0)
	gpi, err := newRow(tel(t, at, &ardupilotmega.MessageGlobalPositionInt{
		Lat: -353632348, Lon: 1491652368, Alt: 604050, RelativeAlt: 19972, Vx: 300, Vy: -400, Hdg: 35994}))
	if err != nil {
		t.Fatal(err)
	}
	if *gpi.lat != -35.3632348 || *gpi.lon != 149.1652368 || *gpi.alt != 604.05 || *gpi.rel != 19.972 ||
		*gpi.speed != 5 || *gpi.hdg != 359.94 || gpi.battery != nil {
		t.Errorf("GLOBAL_POSITION_INT columns: %v %v %v %v %v %v %v", *gpi.lat, *gpi.lon, *gpi.alt, *gpi.rel, *gpi.speed, *gpi.hdg, gpi.battery)
	}
	noHdg, _ := newRow(tel(t, at, &ardupilotmega.MessageGlobalPositionInt{Hdg: 65535}))
	if noHdg.hdg != nil {
		t.Error("unknown heading stored")
	}
	sys, _ := newRow(tel(t, at, &ardupilotmega.MessageSysStatus{BatteryRemaining: 72}))
	unknown, _ := newRow(tel(t, at, &ardupilotmega.MessageBatteryStatus{BatteryRemaining: -1}))
	hb, _ := newRow(tel(t, at, &ardupilotmega.MessageHeartbeat{}))
	if sys.battery == nil || *sys.battery != 72 || unknown.battery != nil || hb.lat != nil || hb.battery != nil {
		t.Errorf("battery columns: %v %v; heartbeat %+v", sys.battery, unknown.battery, hb)
	}
}

// env is a JetStream server, a migrated database and Redis.
type env struct {
	ctx  context.Context
	js   jetstream.JetStream
	pool *pgxpool.Pool
	rdb  *redis.Client
}

func newEnv(t *testing.T) *env {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	t.Cleanup(cancel)
	pool := dbtest.New(t)
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	rdb := newRedis(t)
	nc, err := bus.Connect(natstest.Start(t), "consumer-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := bus.EnsureTelemetryStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	return &env{ctx: ctx, js: js, pool: pool, rdb: rdb}
}

// publishFlight publishes every drone frame of flight.tlog, as the ingester
// would for drone 1, and returns them.
func (e *env) publishFlight(t *testing.T) []schema.Telemetry {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/flight.tlog")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := mavlink.NewDecoder()
	var out []schema.Telemetry
	for _, rec := range recs {
		frames, _ := d.Decode(rec.Time, netip.AddrPort{}, rec.Frame)
		for _, r := range frames {
			if r.SysID != 1 {
				continue
			}
			m, err := schema.FromReceived(1, 1, r)
			if err != nil {
				t.Fatal(err)
			}
			data, _ := json.Marshal(m)
			if _, err := e.js.PublishAsync(m.Subject(), data, jetstream.WithMsgID(m.DedupID())); err != nil {
				t.Fatal(err)
			}
			out = append(out, m)
		}
	}
	select {
	case <-e.js.PublishAsyncComplete():
	case <-e.ctx.Done():
		t.Fatal("publish not acknowledged")
	}
	return out
}

func (e *env) newConsumer(t *testing.T, batch int) *consumer {
	t.Helper()
	cons, err := e.js.CreateOrUpdateConsumer(e.ctx, bus.TelemetryStream, consumerConfig("db-writer", batch))
	if err != nil {
		t.Fatal(err)
	}
	return &consumer{cons: cons, pool: e.pool, rdb: e.rdb, batch: batch, wait: 100 * time.Millisecond}
}

// runUntil runs c until done() is true, then stops it.
func (e *env) runUntil(t *testing.T, c *consumer, done func() bool) {
	t.Helper()
	ctx, cancel := context.WithCancel(e.ctx)
	stopped := make(chan struct{})
	go func() { c.run(ctx); close(stopped) }()
	for !done() {
		select {
		case <-e.ctx.Done():
			cancel()
			t.Fatalf("timed out: %s", c.stats.String())
		case <-time.After(20 * time.Millisecond):
		}
	}
	cancel()
	<-stopped
}

// drained reports whether the durable consumer has nothing left to deliver
// or acknowledge.
func (e *env) drained(c *consumer) bool {
	info, err := c.cons.Info(e.ctx)
	return err == nil && info.NumPending == 0 && info.NumAckPending == 0
}

func (e *env) count(t *testing.T, query string) int64 {
	t.Helper()
	var n int64
	if err := e.pool.QueryRow(e.ctx, query).Scan(&n); err != nil {
		t.Fatal(err)
	}
	return n
}

func (e *env) checkStored(t *testing.T, sent []schema.Telemetry) {
	t.Helper()
	if n := e.count(t, "SELECT count(*) FROM telemetry"); n != int64(len(sent)) {
		t.Errorf("%d rows, want %d", n, len(sent))
	}
	if n := e.count(t, `SELECT count(*) FROM (SELECT 1 FROM telemetry
		GROUP BY drone_id, msg_type, received_at HAVING count(*) > 1) d`); n != 0 {
		t.Errorf("%d duplicate keys", n)
	}
	// Typed columns on every position row, and only there.
	if n := e.count(t, "SELECT count(*) FROM telemetry WHERE lat IS NOT NULL"); n != 793 {
		t.Errorf("%d rows with a position, want 793", n)
	}

	// Redis holds the newest message of each type and the time of the last one.
	h, err := e.rdb.HGetAll(e.ctx, "drone:1:1").Result()
	if err != nil {
		t.Fatal(err)
	}
	last := sent[len(sent)-1]
	if got, want := h["last_seen:us"], itoa(last.ReceivedAt.UnixMicro()); got != want {
		t.Errorf("last_seen:us %s, want %s", got, want)
	}
	var hb schema.Telemetry
	if err := json.Unmarshal([]byte(h["HEARTBEAT"]), &hb); err != nil {
		t.Fatalf("HEARTBEAT in Redis: %v", err)
	}
	for i := len(sent) - 1; i >= 0; i-- {
		if sent[i].Msg == "HEARTBEAT" {
			if !hb.ReceivedAt.Equal(sent[i].ReceivedAt) {
				t.Errorf("Redis HEARTBEAT from %v, want the last one, %v", hb.ReceivedAt, sent[i].ReceivedAt)
			}
			break
		}
	}
}

func itoa(n int64) string { return strconv.FormatInt(n, 10) }

func TestConsumeFlight(t *testing.T) {
	e := newEnv(t)
	sent := e.publishFlight(t)
	c := e.newConsumer(t, 500)
	e.runUntil(t, c, func() bool { return e.drained(c) })
	e.checkStored(t, sent)
	if c.stats.duplicates.Load() != 0 || c.stats.rejected.Load() != 0 || c.stats.failed.Load() != 0 {
		t.Errorf("stats %s", c.stats.String())
	}
}

func TestRestartLosesNothing(t *testing.T) {
	// The Phase 2 promise in miniature: stop the consumer partway, start a new
	// one on the same durable consumer, and every message is stored once.
	e := newEnv(t)
	sent := e.publishFlight(t)

	first := e.newConsumer(t, 100)
	e.runUntil(t, first, func() bool { return first.stats.inserted.Load() >= uint64(len(sent))/3 })
	if n := first.stats.inserted.Load(); n >= uint64(len(sent)) {
		t.Fatalf("first consumer wrote everything (%d) before stopping", n)
	}
	if first.stats.failed.Load() != 0 {
		t.Errorf("stopping abandoned a batch in hand: %s", first.stats.String())
	}

	second := e.newConsumer(t, 100)
	e.runUntil(t, second, func() bool { return e.drained(second) })
	e.checkStored(t, sent)
	t.Logf("first: %s; second: %s", first.stats.String(), second.stats.String())
}

func TestRedisFailureRedelivers(t *testing.T) {
	e := newEnv(t)
	sent := e.publishFlight(t)

	// Redis down: the database write succeeds, but nothing is acknowledged.
	c := e.newConsumer(t, 500)
	good := c.rdb
	c.rdb = redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	defer c.rdb.Close()
	e.runUntil(t, c, func() bool { return c.stats.failed.Load() >= 3 })
	if c.stats.inserted.Load() == 0 {
		t.Fatal("nothing reached the database")
	}

	// Redis back: the batches come again, the database drops what it has,
	// and Redis catches up.
	c.rdb = good
	e.runUntil(t, c, func() bool { return e.drained(c) })
	e.checkStored(t, sent)
	if c.stats.duplicates.Load() == 0 {
		t.Errorf("no redelivered rows: %s", c.stats.String())
	}
	t.Logf("stats: %s", c.stats.String())
}

func TestOlderMessageDoesNotOverwrite(t *testing.T) {
	e := &env{ctx: context.Background(), rdb: newRedis(t)}
	at := time.Unix(1793516806, 0)
	newer := tel(t, at.Add(time.Second), &ardupilotmega.MessageSysStatus{BatteryRemaining: 70})
	older := tel(t, at, &ardupilotmega.MessageSysStatus{BatteryRemaining: 90})
	for _, m := range []schema.Telemetry{newer, older} { // the older one is a late redelivery
		data, _ := json.Marshal(m)
		if err := writeState(e.ctx, e.rdb, []schema.Telemetry{m}, [][]byte{data}); err != nil {
			t.Fatal(err)
		}
	}
	h, _ := e.rdb.HGetAll(e.ctx, stateKey(1, 7)).Result()
	var got schema.Telemetry
	json.Unmarshal([]byte(h["SYS_STATUS"]), &got) //nolint:errcheck
	if !got.ReceivedAt.Equal(newer.ReceivedAt) || h["last_seen:us"] != itoa(newer.ReceivedAt.UnixMicro()) {
		t.Errorf("Redis holds %v (last seen %s), want the newer %v", got.ReceivedAt, h["last_seen:us"], newer.ReceivedAt)
	}
}

func TestConsumerMetrics(t *testing.T) {
	e := newEnv(t)
	sent := e.publishFlight(t)
	c := e.newConsumer(t, 500)
	reg := metrics.NewRegistry()
	c.obs = newObservers(reg, c)
	e.runUntil(t, c, func() bool { return e.drained(c) })
	c.obs.trackLag(canceled(), c, time.Hour) // one refresh, then return

	snap, err := metrics.Snapshot(reg)
	if err != nil {
		t.Fatal(err)
	}
	n := float64(len(sent))
	for name, want := range map[string]float64{
		"murmur_consumer_fetched_total":                   n,
		"murmur_consumer_inserted_total":                  n,
		"murmur_consumer_duplicates_total":                0,
		"murmur_consumer_failed_batches_total":            0,
		"murmur_consumer_lag":                             0,
		"murmur_consumer_batch_size_sum":                  n, // every message in exactly one batch
		"murmur_consumer_receive_to_commit_seconds_count": n, // one latency per message
	} {
		if got := snap[name]; got != want {
			t.Errorf("%s = %v, want %v", name, got, want)
		}
	}
	// The recording is from 2026-11-01, so every latency is at least the age
	// of the recording: proof received_at, not the fetch time, is measured.
	age := time.Since(sent[len(sent)-1].ReceivedAt).Seconds()
	if mean := snap["murmur_consumer_receive_to_commit_seconds_sum"] / n; mean < age {
		t.Errorf("mean latency %.0f s, less than the recording's age %.0f s", mean, age)
	}
	if snap["murmur_consumer_batch_write_seconds_count"] != snap["murmur_consumer_batch_size_count"] {
		t.Errorf("batch write and size counts differ: %v", snap)
	}
}

// canceled returns a context that is already done.
func canceled() context.Context {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	return ctx
}
