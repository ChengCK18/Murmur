package main

import (
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"net/netip"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/bus/natstest"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/tlog"
)

// seedRedis writes drone 1's hash as the consumer would after flight.tlog:
// the newest message of each type.
func seedRedis(t *testing.T, ctx context.Context, rdb *redis.Client) {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/flight.tlog")
	if err != nil {
		t.Fatal(err)
	}
	d, _ := mavlink.NewDecoder()
	hash := map[string]any{}
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
			js, _ := json.Marshal(m)
			hash[m.Msg] = string(js)
			hash[m.Msg+":us"] = m.ReceivedAt.UnixMicro()
		}
	}
	if err := rdb.HSet(ctx, "drone:1:1", hash).Err(); err != nil {
		t.Fatal(err)
	}
}

func TestGateway(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()
	db := 1 + rand.IntN(15)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: db})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("no Redis at 127.0.0.1:6379 (start deploy/docker-compose.yml): %v", err)
	}
	rdb.FlushDB(ctx)
	defer func() { rdb.FlushDB(context.Background()); rdb.Close() }()
	seedRedis(t, ctx, rdb)

	natsURL := natstest.Start(t)
	ready := make(chan string, 1)
	done := make(chan error, 1)
	runCtx, stop := context.WithCancel(ctx)
	go func() {
		done <- run(runCtx, config{
			listen: "127.0.0.1:0", natsURL: natsURL, redisURL: fmt.Sprintf("redis://127.0.0.1:6379/%d", db),
			tenant: 1, throttle: 50 * time.Millisecond, timeout: 3 * time.Second, maxSubs: 100,
			origins: []string{"localhost:5173"},
		}, ready)
	}()
	var addr string
	select {
	case addr = <-ready:
	case err := <-done:
		t.Fatal(err)
	}
	base := "http://" + addr

	// REST: health, and the drone list from Redis (guide step 3.4).
	var health map[string]string
	if code := getJSON(t, base+"/api/health", &health); code != 200 || health["status"] != "ok" {
		t.Errorf("health %d %v", code, health)
	}
	var list struct {
		Drones []map[string]any `json:"drones"`
	}
	getJSON(t, base+"/api/drones", &list)
	if len(list.Drones) != 1 || list.Drones[0]["mode"] != "AUTO" ||
		list.Drones[0]["battery"].(map[string]any)["percent"].(float64) != 72 {
		t.Errorf("drones %v", list.Drones)
	}

	// WebSocket: subscribe, snapshot from Redis, then live updates (3.1, 3.2).
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", nil)
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow() //nolint:errcheck
	send(t, ctx, conn, request{Type: "subscribe", Drones: []int64{1, 2}})
	snap := read(t, ctx, conn)
	drones, _ := snap["drones"].([]any)
	if snap["type"] != "snapshot" || len(drones) != 1 || drones[0].(map[string]any)["drone_id"].(float64) != 1 {
		t.Fatalf("snapshot %v", snap)
	}

	nc, err := bus.Connect(natsURL, "test")
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	js, _ := jetstream.New(nc)
	// The feed only sees messages published after it starts, so publish
	// until drone 2 appears.
	var update map[string]any
	for update == nil {
		hb := tel(t, 2, time.Now(), heartbeat())
		data, _ := json.Marshal(hb)
		if _, err := js.Publish(ctx, hb.Subject(), data); err != nil {
			t.Fatal(err)
		}
		readCtx, c := context.WithTimeout(ctx, 200*time.Millisecond)
		var m map[string]any
		if err := wsjson.Read(readCtx, conn, &m); err == nil {
			update = m
		}
		c()
		if ctx.Err() != nil {
			t.Fatal("no update for drone 2")
		}
	}
	st := update["state"].(map[string]any)
	if update["type"] != "update" || update["drone_id"].(float64) != 2 || st["mode"] != "GUIDED" || st["link"] != "ok" {
		t.Errorf("update %v", update)
	}

	// Bad requests get an error, and the connection stays open.
	send(t, ctx, conn, request{Type: "dance"})
	if m := read(t, ctx, conn); m["type"] != "error" {
		t.Errorf("unknown request: %v", m)
	}

	// Only the gateway's own origin and the Vite dev server may connect.
	_, resp, err := websocket.Dial(ctx, "ws://"+addr+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://evil.example"}}})
	if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
		t.Errorf("foreign origin: err %v, response %v", err, resp)
	}
	c2, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", &websocket.DialOptions{
		HTTPHeader: http.Header{"Origin": {"http://localhost:5173"}}})
	if err != nil {
		t.Errorf("Vite dev server origin refused: %v", err)
	} else {
		c2.CloseNow() //nolint:errcheck
	}

	stop()
	if err := <-done; err != nil {
		t.Errorf("run: %v", err)
	}
}

func getJSON(t *testing.T, url string, v any) int {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if err := json.NewDecoder(resp.Body).Decode(v); err != nil {
		t.Fatalf("%s: %v", url, err)
	}
	return resp.StatusCode
}

func send(t *testing.T, ctx context.Context, conn *websocket.Conn, req request) {
	t.Helper()
	if err := wsjson.Write(ctx, conn, req); err != nil {
		t.Fatal(err)
	}
}

func read(t *testing.T, ctx context.Context, conn *websocket.Conn) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	var m map[string]any
	if err := wsjson.Read(ctx, conn, &m); err != nil {
		t.Fatal(err)
	}
	return m
}
