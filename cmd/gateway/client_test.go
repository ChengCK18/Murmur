package main

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/state"
)

var t0 = time.Unix(1793516806, 0)

func tel(t *testing.T, drone int64, at time.Time, m message.Message) schema.Telemetry {
	t.Helper()
	s, err := schema.FromReceived(1, drone, mavlink.Received{ReceivedAt: at, SysID: uint8(drone), CompID: 1, Msg: m})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func heartbeat() message.Message {
	return &ardupilotmega.MessageHeartbeat{Autopilot: ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA, CustomMode: 4, BaseMode: 217}
}

func position(lat int32) message.Message {
	return &ardupilotmega.MessageGlobalPositionInt{Lat: lat, Lon: 1491652368, RelativeAlt: 20000, Hdg: 9000}
}

// recorder is a browser: it keeps every message the gateway writes.
type recorder struct {
	mu   sync.Mutex
	msgs []map[string]any
	at   []time.Time
}

func (r *recorder) send(_ context.Context, b []byte) error {
	var m map[string]any
	if err := json.Unmarshal(b, &m); err != nil {
		return err
	}
	r.mu.Lock()
	r.msgs = append(r.msgs, m)
	r.at = append(r.at, time.Now())
	r.mu.Unlock()
	return nil
}

func (r *recorder) messages() []map[string]any {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]map[string]any(nil), r.msgs...)
}

// noRedis is a Redis client that fails at once: these tests need only the
// live feed.
func noRedis() *redis.Client {
	return redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
}

func connect(t *testing.T, h *hub, throttle time.Duration, send func(context.Context, []byte) error) (*client, *counters) {
	t.Helper()
	counts := &counters{}
	cl := newClient(send, throttle, counts)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	go cl.run(ctx) //nolint:errcheck
	return cl, counts
}

func TestThrottle(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	rec := &recorder{}
	cl, counts := connect(t, h, 50*time.Millisecond, rec.send)
	h.subscribe(context.Background(), cl, []int64{1})

	// 200 positions in half a second (400 per second), far more than the
	// 20 per second a 50 ms throttle allows.
	start := time.Now()
	for i := range 200 {
		h.apply(tel(t, 1, t0.Add(time.Duration(i)*time.Millisecond), position(int32(i))), t0)
		time.Sleep(2500 * time.Microsecond)
	}
	elapsed := time.Since(start)
	time.Sleep(120 * time.Millisecond) // the last tick

	msgs := rec.messages()
	if msgs[0]["type"] != "snapshot" {
		t.Fatalf("first message %v, want the snapshot", msgs[0]["type"])
	}
	updates := msgs[1:]
	if max := int(elapsed/(50*time.Millisecond)) + 2; len(updates) > max || len(updates) < 3 {
		t.Errorf("%d updates in %v, want between 3 and %d", len(updates), elapsed, max)
	}
	last := updates[len(updates)-1]["state"].(map[string]any)["position"].(map[string]any)
	if last["lat"].(float64) != 199e-7 {
		t.Errorf("last update lat %v, want the newest (199e-7)", last["lat"])
	}
	if counts.coalesced.Load() < 150 {
		t.Errorf("only %d updates coalesced", counts.coalesced.Load())
	}
}

func TestStuckBrowserBlocksNobody(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	stuck := make(chan struct{}) // never closed: every write hangs
	slow, _ := connect(t, h, 10*time.Millisecond, func(ctx context.Context, b []byte) error {
		select {
		case <-stuck:
		case <-ctx.Done():
		}
		return ctx.Err()
	})
	rec := &recorder{}
	fast, _ := connect(t, h, 10*time.Millisecond, rec.send)
	h.subscribe(context.Background(), slow, []int64{1})
	h.subscribe(context.Background(), fast, []int64{1})

	start := time.Now()
	for i := range 1000 {
		h.apply(tel(t, 1, t0.Add(time.Duration(i)*time.Millisecond), position(int32(i))), t0)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("1000 updates took %v with a stuck browser subscribed", took)
	}
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.messages()); n < 2 {
		t.Errorf("the other browser got %d messages", n)
	}
	// The stuck browser holds one pending state per drone, not a queue.
	slow.mu.Lock()
	pending := len(slow.pending)
	slow.mu.Unlock()
	if pending > 1 {
		t.Errorf("stuck browser has %d pending states, want at most 1", pending)
	}
}

func TestSnapshotThenUpdates(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	h.apply(tel(t, 1, t0, heartbeat()), t0)
	h.apply(tel(t, 1, t0, position(1)), t0)

	rec := &recorder{}
	cl, _ := connect(t, h, 20*time.Millisecond, rec.send)
	h.subscribe(context.Background(), cl, []int64{1, 2}) // drone 2 has no state yet
	h.apply(tel(t, 1, t0.Add(time.Second), position(2)), t0)
	h.apply(tel(t, 2, t0.Add(time.Second), heartbeat()), t0)
	time.Sleep(80 * time.Millisecond)

	msgs := rec.messages()
	if len(msgs) != 3 || msgs[0]["type"] != "snapshot" {
		t.Fatalf("messages %v", msgs)
	}
	snap := msgs[0]["drones"].([]any)
	if len(snap) != 1 || snap[0].(map[string]any)["mode"] != "GUIDED" {
		t.Errorf("snapshot %v, want drone 1 only", snap)
	}
	for _, m := range msgs[1:] {
		if m["type"] != "update" {
			t.Errorf("then %v, want updates", m["type"])
		}
	}

	// Unsubscribed: nothing more for drone 1.
	h.unsubscribe(cl, []int64{1})
	h.apply(tel(t, 1, t0.Add(2*time.Second), position(3)), t0)
	time.Sleep(50 * time.Millisecond)
	if n := len(rec.messages()); n != 3 {
		t.Errorf("%d messages after unsubscribing, want 3", n)
	}
}

func TestSubscriptionLimit(t *testing.T) {
	h := newHub(1, 3*time.Second, 2, noRedis())
	cl, _ := connect(t, h, time.Hour, (&recorder{}).send)
	if refused := h.subscribe(context.Background(), cl, []int64{1, 2, 3}); refused != 1 {
		t.Errorf("refused %d, want 1", refused)
	}
	if refused := h.subscribe(context.Background(), cl, []int64{1, 2}); refused != 0 {
		t.Errorf("subscribing again to the same drones refused %d", refused)
	}
	h.unsubscribe(cl, []int64{1})
	if refused := h.subscribe(context.Background(), cl, []int64{3}); refused != 0 {
		t.Errorf("a freed slot refused %d", refused)
	}
	h.remove(cl)
	if len(h.subs) != 0 {
		t.Errorf("subscriptions left after the browser went: %v", h.subs)
	}
}

func TestLinkLostIsSent(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	rec := &recorder{}
	cl, _ := connect(t, h, 10*time.Millisecond, rec.send)
	h.subscribe(context.Background(), cl, []int64{1})
	h.apply(tel(t, 1, t0, heartbeat()), t0)
	time.Sleep(40 * time.Millisecond)     // sent; otherwise the throttle would merge it with the next
	h.checkLinks(t0.Add(2 * time.Second)) // still fine: nothing to send
	h.checkLinks(t0.Add(4 * time.Second)) // silent for 4 s: lost
	time.Sleep(40 * time.Millisecond)

	var links []string
	for _, m := range rec.messages()[1:] {
		links = append(links, m["state"].(map[string]any)["link"].(string))
	}
	if len(links) != 2 || links[0] != "ok" || links[1] != "lost" {
		t.Errorf("link states sent %v, want [ok lost]", links)
	}
}

func TestStaleDronesRemoved(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	h.stale = time.Minute
	now := time.Now()
	h.apply(tel(t, 1, now.Add(-2*time.Minute), heartbeat()), now) // from a past run
	h.apply(tel(t, 2, now, heartbeat()), now)

	rec := &recorder{}
	cl, _ := connect(t, h, 10*time.Millisecond, rec.send)
	h.subscribe(context.Background(), cl, []int64{1, 2})
	time.Sleep(30 * time.Millisecond)
	if snap := rec.messages()[0]["drones"].([]any); len(snap) != 1 || snap[0].(map[string]any)["drone_id"].(float64) != 2 {
		t.Fatalf("snapshot %v, want drone 2 only", snap)
	}

	// Drone 2 goes quiet: link lost after 3 s, removed after a minute, once.
	h.checkLinks(now.Add(30 * time.Second))
	time.Sleep(30 * time.Millisecond)
	h.checkLinks(now.Add(2 * time.Minute))
	time.Sleep(30 * time.Millisecond)
	h.checkLinks(now.Add(3 * time.Minute))
	time.Sleep(30 * time.Millisecond)
	// Back again: shown again.
	h.apply(tel(t, 2, now.Add(4*time.Minute), heartbeat()), now.Add(4*time.Minute))
	time.Sleep(30 * time.Millisecond)

	var got []string
	for _, m := range rec.messages()[1:] {
		s := m["type"].(string)
		if st, ok := m["state"].(map[string]any); ok {
			s += " " + st["link"].(string)
		}
		got = append(got, s)
	}
	if want := "[update lost remove update ok]"; fmt.Sprint(got) != want {
		t.Errorf("messages %v, want %s", got, want)
	}
}

func TestRemoveDoesNotOvertakeUpdate(t *testing.T) {
	// An update and then a removal before the next send: the removal wins,
	// in the same slot, and is the only message.
	counts := &counters{}
	rec := &recorder{}
	cl := newClient(rec.send, time.Hour, counts)
	cl.offer(stateOf(t, 5))
	cl.offerRemove(5)
	if err := cl.flush(context.Background(), true, true); err != nil {
		t.Fatal(err)
	}
	if msgs := rec.messages(); len(msgs) != 1 || msgs[0]["type"] != "remove" || msgs[0]["drone_id"].(float64) != 5 {
		t.Errorf("messages %v, want one removal of drone 5", msgs)
	}
}

func stateOf(t *testing.T, id int64) *state.Drone {
	t.Helper()
	d := state.New(1, id)
	if _, err := d.Apply(tel(t, id, t0, heartbeat())); err != nil {
		t.Fatal(err)
	}
	return d
}
