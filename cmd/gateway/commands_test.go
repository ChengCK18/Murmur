package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"math/rand/v2"
	"net/http"
	"testing"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus/natstest"
	"github.com/ChengCK18/murmur/internal/command"
)

// commandGateway runs a gateway with commands on, against a real NATS and
// Redis, and returns its address and a NATS connection to play the commander.
func commandGateway(t *testing.T, ctx context.Context, cfg config) (string, *nats.Conn) {
	t.Helper()
	db := 1 + rand.IntN(15)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: db})
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("no Redis at 127.0.0.1:6379 (start deploy/docker-compose.yml): %v", err)
	}
	t.Cleanup(func() { rdb.FlushDB(context.Background()); rdb.Close() })

	natsURL := natstest.Start(t)
	cfg.listen, cfg.natsURL, cfg.redisURL = "127.0.0.1:0", natsURL, fmt.Sprintf("redis://127.0.0.1:6379/%d", db)
	cfg.tenant, cfg.throttle, cfg.timeout, cfg.maxSubs = 1, 50*time.Millisecond, 3*time.Second, 100
	ready := make(chan string, 1)
	done := make(chan error, 1)
	go func() { done <- run(ctx, cfg, ready) }()
	select {
	case addr := <-ready:
		nc, err := nats.Connect(natsURL)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(nc.Close)
		return addr, nc
	case err := <-done:
		t.Fatal(err)
		return "", nil
	}
}

func login(t *testing.T, addr, user, password string) (*http.Cookie, int) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": user, "password": password})
	res, err := http.Post("http://"+addr+"/api/login", "application/json", bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	for _, c := range res.Cookies() {
		if c.Name == sessionCookie {
			return c, res.StatusCode
		}
	}
	return nil, res.StatusCode
}

func dial(t *testing.T, ctx context.Context, addr string, cookie *http.Cookie) *websocket.Conn {
	t.Helper()
	opts := &websocket.DialOptions{}
	if cookie != nil {
		opts.HTTPHeader = http.Header{"Cookie": {cookie.Name + "=" + cookie.Value}}
	}
	conn, _, err := websocket.Dial(ctx, "ws://"+addr+"/ws", opts)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { conn.CloseNow() }) //nolint:errcheck
	return conn
}

func nextStatus(t *testing.T, ctx context.Context, conn *websocket.Conn) statusMsg {
	t.Helper()
	wait, cancel := context.WithTimeout(ctx, 5*time.Second)
	defer cancel()
	for {
		var m statusMsg
		if err := wsjson.Read(wait, conn, &m); err != nil {
			t.Fatalf("no status: %v", err)
		}
		if m.Type == "command_status" {
			return m
		}
	}
}

func TestLogin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, _ := commandGateway(t, ctx, config{commands: true, user: "demo", password: "secret"})

	if c, code := login(t, addr, "demo", "wrong"); code != http.StatusUnauthorized || c != nil {
		t.Errorf("wrong password: %d, cookie %v", code, c)
	}
	if c, code := login(t, addr, "other", "secret"); code != http.StatusUnauthorized || c != nil {
		t.Errorf("wrong user: %d, cookie %v", code, c)
	}
	c, code := login(t, addr, "demo", "secret")
	if code != http.StatusOK || c == nil {
		t.Fatalf("login: %d, cookie %v", code, c)
	}
	if !c.HttpOnly || c.SameSite != http.SameSiteStrictMode || c.MaxAge != int(sessionLife.Seconds()) {
		t.Errorf("cookie %+v", c)
	}

	who := func(c *http.Cookie) string {
		req, _ := http.NewRequest("GET", "http://"+addr+"/api/session", nil)
		if c != nil {
			req.AddCookie(c)
		}
		res, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		defer res.Body.Close()
		var body map[string]string
		json.NewDecoder(res.Body).Decode(&body) //nolint:errcheck
		return body["user"]
	}
	if who(c) != "demo" || who(nil) != "" {
		t.Errorf("session: %q, without a cookie %q", who(c), who(nil))
	}
	if who(&http.Cookie{Name: sessionCookie, Value: "made-up"}) != "" {
		t.Error("a made-up token was accepted")
	}

	req, _ := http.NewRequest("POST", "http://"+addr+"/api/logout", nil)
	req.AddCookie(c)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	res.Body.Close()
	if who(c) != "" {
		t.Error("the session outlived the logout")
	}
}

func TestNoPasswordMeansNoLogins(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, _ := commandGateway(t, ctx, config{commands: true, user: "demo"})
	if c, code := login(t, addr, "demo", ""); code != http.StatusUnauthorized || c != nil {
		t.Errorf("empty password: %d, cookie %v", code, c)
	}
}

func TestCommandNeedsALogin(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, nc := commandGateway(t, ctx, config{commands: true, user: "demo", password: "secret"})
	seen := make(chan struct{}, 1)
	if _, err := nc.Subscribe(command.RequestSubject, func(*nats.Msg) { seen <- struct{}{} }); err != nil {
		t.Fatal(err)
	}
	nc.Flush() //nolint:errcheck

	conn := dial(t, ctx, addr, nil)
	send(t, ctx, conn, request{Type: "command", RequestID: "r1", DroneID: 1, Command: command.Arm})
	m := nextStatus(t, ctx, conn)
	if m.RequestID != "r1" || m.Update.Status != command.Rejected || m.Update.Reason != "log in to send commands" {
		t.Errorf("%+v", m)
	}
	select {
	case <-seen:
		t.Error("the command reached the commander")
	case <-time.After(100 * time.Millisecond):
	}
}

func TestCommandsSwitchedOff(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, _ := commandGateway(t, ctx, config{user: "demo", password: "secret"})
	c, _ := login(t, addr, "demo", "secret")
	conn := dial(t, ctx, addr, c)
	send(t, ctx, conn, request{Type: "command", RequestID: "r1", DroneID: 1, Command: command.Arm})
	if m := nextStatus(t, ctx, conn); m.Update.Status != command.Rejected || m.Update.Reason != "commands are switched off" {
		t.Errorf("%+v", m)
	}
}

func TestCommandStatusReachesTheBrowsersThatCare(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, nc := commandGateway(t, ctx, config{commands: true, user: "demo", password: "secret"})

	var req command.Request
	got := make(chan struct{}, 1)
	if _, err := nc.Subscribe(command.RequestSubject, func(m *nats.Msg) {
		json.Unmarshal(m.Data, &req) //nolint:errcheck
		data, _ := json.Marshal(command.Update{CommandID: "c1", DroneID: req.DroneID, Type: req.Type, Status: command.Pending, IssuedBy: req.IssuedBy})
		m.Respond(data) //nolint:errcheck
		got <- struct{}{}
	}); err != nil {
		t.Fatal(err)
	}
	nc.Flush() //nolint:errcheck

	c, _ := login(t, addr, "demo", "secret")
	issuer := dial(t, ctx, addr, c)
	watcher := dial(t, ctx, addr, nil)
	bystander := dial(t, ctx, addr, nil)
	send(t, ctx, watcher, request{Type: "subscribe", Drones: []int64{42}})
	send(t, ctx, bystander, request{Type: "subscribe", Drones: []int64{43}})
	read(t, ctx, watcher) // their snapshots: the subscriptions are in place
	read(t, ctx, bystander)

	send(t, ctx, issuer, request{Type: "command", RequestID: "r1", DroneID: 42, Command: command.Takeoff, Params: command.Params{AltitudeM: 20}})
	first := nextStatus(t, ctx, issuer)
	if first.RequestID != "r1" || first.Update.Status != command.Pending || first.Update.CommandID != "c1" {
		t.Errorf("first status %+v", first)
	}
	<-got
	// The user comes from the session; the browser has no say in it.
	if req.IssuedBy != "demo" || req.Params.AltitudeM != 20 || req.DroneID != 42 {
		t.Errorf("forwarded %+v", req)
	}

	data, _ := json.Marshal(command.Update{CommandID: "c1", DroneID: 42, Type: command.Takeoff, Status: command.Accepted, IssuedBy: "demo"})
	if err := nc.Publish(command.StatusSubject(42), data); err != nil {
		t.Fatal(err)
	}
	if m := nextStatus(t, ctx, issuer); m.Update.Status != command.Accepted {
		t.Errorf("issuer: %+v", m)
	}
	if m := nextStatus(t, ctx, watcher); m.Update.Status != command.Accepted {
		t.Errorf("watcher: %+v", m)
	}
	quiet, cancelQuiet := context.WithTimeout(ctx, 200*time.Millisecond)
	defer cancelQuiet()
	var other statusMsg
	if err := wsjson.Read(quiet, bystander, &other); err == nil && other.Type == "command_status" {
		t.Errorf("a browser watching another drone was told: %+v", other)
	}
}

func TestSilentCommanderIsReported(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, _ := commandGateway(t, ctx, config{commands: true, user: "demo", password: "secret"})
	c, _ := login(t, addr, "demo", "secret")
	conn := dial(t, ctx, addr, c)
	send(t, ctx, conn, request{Type: "command", RequestID: "r1", DroneID: 1, Command: command.Arm})
	if m := nextStatus(t, ctx, conn); m.Update.Status != command.Rejected || m.Update.Reason != "the command service is not answering" {
		t.Errorf("%+v", m)
	}
}

func TestRecentCommands(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, nc := commandGateway(t, ctx, config{commands: true, user: "demo", password: "secret"})
	if _, err := nc.Subscribe(command.RecentSubject, func(m *nats.Msg) {
		var r command.RecentRequest
		json.Unmarshal(m.Data, &r) //nolint:errcheck
		data, _ := json.Marshal([]command.Update{{CommandID: "c1", DroneID: r.DroneID, Status: command.Unknown}})
		m.Respond(data) //nolint:errcheck
	}); err != nil {
		t.Fatal(err)
	}
	nc.Flush() //nolint:errcheck

	var body struct {
		Commands []command.Update `json:"commands"`
	}
	if code := getJSON(t, "http://"+addr+"/api/drones/42/commands", &body); code != http.StatusUnauthorized {
		t.Errorf("without a login: %d", code)
	}
	c, _ := login(t, addr, "demo", "secret")
	req, _ := http.NewRequest("GET", "http://"+addr+"/api/drones/42/commands", nil)
	req.AddCookie(c)
	res, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer res.Body.Close()
	if err := json.NewDecoder(res.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if res.StatusCode != http.StatusOK || len(body.Commands) != 1 || body.Commands[0].DroneID != 42 {
		t.Errorf("%d %+v", res.StatusCode, body)
	}
}

func TestFleetCommand(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	addr, nc := commandGateway(t, ctx, config{commands: true, user: "demo", password: "secret"})
	var req command.FleetRequest
	if _, err := nc.Subscribe(command.FleetSubject, func(m *nats.Msg) {
		json.Unmarshal(m.Data, &req) //nolint:errcheck
		data, _ := json.Marshal(command.FleetStarted{FleetID: "f1", Count: len(req.DroneIDs)})
		m.Respond(data) //nolint:errcheck
	}); err != nil {
		t.Fatal(err)
	}
	nc.Flush() //nolint:errcheck

	next := func(conn *websocket.Conn) fleetMsg {
		wait, cancel := context.WithTimeout(ctx, 5*time.Second)
		defer cancel()
		var m fleetMsg
		if err := wsjson.Read(wait, conn, &m); err != nil {
			t.Fatal(err)
		}
		return m
	}

	anon := dial(t, ctx, addr, nil)
	send(t, ctx, anon, request{Type: "fleet_command", RequestID: "r0", Drones: []int64{1}, Command: command.Land})
	if m := next(anon); m.Type != "fleet_started" || m.Error != "log in to send commands" {
		t.Errorf("without a login: %+v", m)
	}

	c, _ := login(t, addr, "demo", "secret")
	conn := dial(t, ctx, addr, c)
	send(t, ctx, conn, request{Type: "fleet_command", RequestID: "r1", Drones: []int64{1, 2, 3}, Command: command.ReturnHome})
	if m := next(conn); m.RequestID != "r1" || m.FleetID != "f1" || m.Count != 3 {
		t.Errorf("%+v", m)
	}
	if req.IssuedBy != "demo" || req.Type != command.ReturnHome || len(req.DroneIDs) != 3 {
		t.Errorf("forwarded %+v", req)
	}
}
