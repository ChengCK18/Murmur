//go:build integration

package integration

import (
	"context"
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/alert"
	"github.com/ChengCK18/murmur/internal/bus/natstest"
	"github.com/ChengCK18/murmur/internal/command"
	"github.com/ChengCK18/murmur/internal/db/dbtest"
	"github.com/ChengCK18/murmur/internal/schema"
)

// TestCommandsReachASITLDrone runs guide step 5.1's first test against real
// ArduCopter: commands go through the commander and the ingester's socket to
// the address SITL sends from. It also holds the line that matters most (plan
// §6): a command sent while the ingester is down never runs, not even after
// the ingester comes back.
func TestCommandsReachASITLDrone(t *testing.T) {
	home, _ := os.UserHomeDir()
	ardupilot := filepath.Join(home, "ardupilot")
	copter := filepath.Join(ardupilot, "build/sitl/bin/arducopter")
	if _, err := os.Stat(copter); err != nil {
		t.Skipf("no SITL at %s", copter)
	}
	repo, _ := filepath.Abs("..")

	bin := build(t, "ingester", "commander", "consumer")
	natsURL := natstest.Start(t)
	_, dbURL := dbtest.NewURL(t)
	redisURL, _ := spareRedis(t)
	udp := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Minute)
	defer cancel()

	ingesterArgs := []string{"-listen", udp, "-nats", natsURL, "-db", dbURL, "-metrics", "", "-interval", "1h"}
	ingester := start(t, bin, "ingester", ingesterArgs...)
	start(t, bin, "consumer", "-nats", natsURL, "-db", dbURL, "-redis", redisURL, "-metrics", "", "-interval", "1h")
	start(t, bin, "commander", "-nats", natsURL, "-db", dbURL, "-redis", redisURL, "-metrics", "")

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	// The drone's ID and armed state, from its heartbeats.
	var drone, armed, beats, reached, lat, lon atomic.Int64
	reached.Store(-1)
	var usingGPS atomic.Bool
	if _, err := nc.Subscribe("telemetry.*", func(m *nats.Msg) {
		var tm schema.Telemetry
		if json.Unmarshal(m.Data, &tm) != nil {
			return
		}
		switch tm.Msg {
		case "MISSION_ITEM_REACHED":
			var p struct {
				Seq int64 `json:"seq"`
			}
			if json.Unmarshal(tm.Payload, &p) == nil {
				reached.Store(p.Seq)
			}
			return
		case "GLOBAL_POSITION_INT":
			var p struct {
				Lat, Lon int64
			}
			if json.Unmarshal(tm.Payload, &p) == nil {
				lat.Store(p.Lat)
				lon.Store(p.Lon)
			}
			return
		case "STATUSTEXT":
			var p struct {
				Text string `json:"text"`
			}
			if json.Unmarshal(tm.Payload, &p) == nil {
				t.Log("drone says: " + p.Text)
				if strings.Contains(p.Text, "is using GPS") {
					usingGPS.Store(true)
				}
			}
			return
		case "HEARTBEAT":
		default:
			return
		}
		var hb struct {
			BaseMode int `json:"base_mode"`
		}
		if json.Unmarshal(tm.Payload, &hb) != nil {
			return
		}
		drone.Store(tm.DroneID)
		armed.Store(int64(hb.BaseMode >> 7 & 1)) // MAV_MODE_FLAG_SAFETY_ARMED
		beats.Add(1)
	}); err != nil {
		t.Fatal(err)
	}

	sitl := exec.Command(copter, "--model", "+", "-I", "5", "--sysid", "1",
		"--defaults", filepath.Join(ardupilot, "Tools/autotest/default_params/copter.parm")+","+filepath.Join(repo, "deploy/sitl/murmur-streams.parm"),
		"--serial0", "udpclient:"+udp)
	sitl.Dir = t.TempDir()
	if err := sitl.Start(); err != nil {
		t.Fatal(err)
	}
	defer func() { sitl.Process.Kill(); sitl.Wait() }() //nolint:errcheck

	waitFor(t, ctx, "the first heartbeat", func() bool { return drone.Load() != 0 })
	id := drone.Load()

	finals := make(chan command.Update, 16)
	if _, err := nc.Subscribe(command.StatusSubject(id), func(m *nats.Msg) {
		var u command.Update
		if json.Unmarshal(m.Data, &u) == nil && command.Final(u.Status) {
			finals <- u
		}
	}); err != nil {
		t.Fatal(err)
	}
	if err := nc.Flush(); err != nil {
		t.Fatal(err)
	}
	issue := func(typ string) command.Update {
		t.Helper()
		data, _ := json.Marshal(command.Request{DroneID: id, Type: typ, IssuedBy: "integration"})
		m, err := nc.Request(command.RequestSubject, data, 2*time.Second)
		if err != nil {
			t.Fatalf("%s: %v", typ, err)
		}
		var first command.Update
		if err := json.Unmarshal(m.Data, &first); err != nil {
			t.Fatal(err)
		}
		if first.Status != command.Pending {
			t.Fatalf("%s refused: %+v", typ, first)
		}
		select {
		case u := <-finals:
			return u
		case <-time.After(10 * time.Second):
			t.Fatalf("%s: no final status", typ)
			return command.Update{}
		}
	}

	// Pre-arm checks refuse until the simulated GPS and EKF have settled, and
	// a refusal is itself proof that the drone hears us.
	var u command.Update
	refusals := 0
	for {
		u = issue(command.Arm)
		if u.Status == command.Accepted {
			break
		}
		if u.Status != command.Failed && u.Status != command.Rejected {
			t.Fatalf("arm: %+v", u)
		}
		refusals++
		select {
		case <-ctx.Done():
			t.Fatalf("still refused after %d tries: %+v", refusals, u)
		case <-time.After(3 * time.Second):
		}
	}
	t.Logf("armed after %d refusals by pre-arm checks", refusals)
	waitFor(t, ctx, "a heartbeat showing armed", func() bool { return armed.Load() == 1 })

	if u = issue(command.Disarm); u.Status != command.Accepted {
		t.Fatalf("disarm: %+v", u)
	}
	waitFor(t, ctx, "a heartbeat showing disarmed", func() bool { return armed.Load() == 0 })

	// The ingester goes away: the command fails fast and is gone for good.
	if err := ingester.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	ingester.Wait() //nolint:errcheck
	at := time.Now()
	u = issue(command.Arm)
	if took := time.Since(at); u.Status != command.Unreachable || took > 1500*time.Millisecond {
		t.Fatalf("arm with the ingester down: %+v after %v", u, took)
	}

	start(t, bin, "ingester", ingesterArgs...)
	n := beats.Load()
	waitFor(t, ctx, "heartbeats after the restart", func() bool { return beats.Load() > n+1 })
	time.Sleep(command.Lifetime + time.Second) // past the moment it could have run
	if armed.Load() != 0 {
		t.Fatal("a command sent while the ingester was down ran later")
	}
	if u = issue(command.Arm); u.Status != command.Accepted {
		t.Fatalf("arm after the restart: %+v", u)
	}
	waitFor(t, ctx, "a heartbeat showing armed again", func() bool { return armed.Load() == 1 })

	// A mission, from the handshake to the drone flying it: two points about
	// 30 m apart near where it stands.
	here := command.Waypoint{Lat: float64(lat.Load()) / 1e7, Lon: float64(lon.Load()) / 1e7}
	data, _ := json.Marshal(command.MissionRequest{DroneID: id, AltitudeM: 10, IssuedBy: "integration", Waypoints: []command.Waypoint{
		{Lat: here.Lat + 0.0002, Lon: here.Lon}, {Lat: here.Lat + 0.0002, Lon: here.Lon + 0.0003},
	}})
	m, err := nc.Request(command.MissionSubject, data, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var first command.Update
	if err := json.Unmarshal(m.Data, &first); err != nil || first.Status != command.Pending {
		t.Fatalf("upload refused: %+v, err %v", first, err)
	}
	select {
	case u = <-finals:
	case <-time.After(10 * time.Second):
		t.Fatal("the upload never ended")
	}
	if u.Type != command.Upload || u.Status != command.Accepted {
		t.Fatalf("upload: %+v", u)
	}
	t.Logf("mission uploaded in %d packets", u.Attempts)

	// ArduPilot refuses AUTO until the EKF is using GPS, some time after it
	// allows arming, and disarms by itself if left idle on the ground.
	waitFor(t, ctx, "the EKF to use GPS", func() bool { return usingGPS.Load() })
	if armed.Load() == 0 {
		if u = issue(command.Arm); u.Status != command.Accepted {
			t.Fatalf("arm: %+v", u)
		}
		waitFor(t, ctx, "a heartbeat showing armed", func() bool { return armed.Load() == 1 })
	}
	time.Sleep(1500 * time.Millisecond) // armed reaches Redis, and so the precondition
	if u = issue(command.MissionStart); u.Status != command.Accepted {
		t.Fatalf("start mission: %+v", u)
	}
	const lastWaypoint = 3 // home, takeoff, then the two points
	waitFor(t, ctx, "the drone to reach the last waypoint", func() bool { return reached.Load() >= lastWaypoint })
}

// TestFleetCommandEndsForEveryDrone sends return to launch to 20 fake drones
// that lose and reject some commands, and checks the plan's limit: every drone
// shows a final status within 5 seconds.
func TestFleetCommandEndsForEveryDrone(t *testing.T) {
	const drones = 20
	bin := build(t, "ingester", "commander", "consumer", "fakegen")
	natsURL := natstest.Start(t)
	_, dbURL := dbtest.NewURL(t)
	redisURL, _ := spareRedis(t)
	udp := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start(t, bin, "ingester", "-listen", udp, "-nats", natsURL, "-db", dbURL, "-metrics", "", "-interval", "1h")
	start(t, bin, "consumer", "-nats", natsURL, "-db", dbURL, "-redis", redisURL, "-metrics", "", "-interval", "1h")
	start(t, bin, "commander", "-nats", natsURL, "-db", dbURL, "-redis", redisURL, "-metrics", "")
	time.Sleep(time.Second) // listening, and the tables created
	start(t, bin, "fakegen", "-count", strconv.Itoa(drones), "-target", udp, "-command-drop", "0.3", "-command-reject", "0.2")

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()

	// Wait until the commander can see every drone flying.
	rdb := redis.NewClient(mustRedisOptions(t, redisURL))
	defer rdb.Close()
	waitFor(t, ctx, "every drone's state", func() bool {
		keys, err := rdb.Keys(ctx, "drone:1:*").Result()
		if err != nil || len(keys) != drones {
			return false
		}
		for _, k := range keys {
			if n, err := rdb.HExists(ctx, k, "GLOBAL_POSITION_INT").Result(); err != nil || !n {
				return false
			}
		}
		return true
	})
	var ids []int64
	keys, _ := rdb.Keys(ctx, "drone:1:*").Result()
	for _, k := range keys {
		id, err := strconv.ParseInt(strings.TrimPrefix(k, "drone:1:"), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, id)
	}

	finals := make(chan command.Update, drones*2)
	if _, err := nc.Subscribe(command.StatusSubjects, func(m *nats.Msg) {
		var u command.Update
		if json.Unmarshal(m.Data, &u) == nil && command.Final(u.Status) {
			finals <- u
		}
	}); err != nil {
		t.Fatal(err)
	}
	nc.Flush() //nolint:errcheck

	data, _ := json.Marshal(command.FleetRequest{DroneIDs: ids, Type: command.ReturnHome, IssuedBy: "integration"})
	at := time.Now()
	m, err := nc.Request(command.FleetSubject, data, 2*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	var started command.FleetStarted
	if err := json.Unmarshal(m.Data, &started); err != nil || started.Error != "" || started.Count != drones {
		t.Fatalf("started %+v, err %v", started, err)
	}

	got := map[int64]string{}
	totals := map[string]int{}
	for len(got) < drones {
		select {
		case u := <-finals:
			if u.FleetID != started.FleetID {
				t.Errorf("wrong fleet: %+v", u)
			}
			if _, dup := got[u.DroneID]; dup {
				t.Errorf("two final statuses for drone %d", u.DroneID)
			}
			got[u.DroneID] = u.Status
			totals[u.Status]++
		case <-time.After(time.Until(at.Add(command.Lifetime + time.Second))):
			t.Fatalf("after %v only %d of %d final: %v", time.Since(at), len(got), drones, totals)
		}
	}
	took := time.Since(at)
	t.Logf("%d drones final in %v: %v", drones, took, totals)
	if took > command.Lifetime {
		t.Errorf("took %v, limit %v", took, command.Lifetime)
	}
}

func mustRedisOptions(t *testing.T, url string) *redis.Options {
	t.Helper()
	o, err := redis.ParseURL(url)
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// TestAlerts runs the alerter on a fake fleet: a geofence that leaves every
// drone outside, a battery level every drone is below, and the fleet stopping.
// Every alert is raised once per drone, and written to the events table.
func TestAlerts(t *testing.T) {
	const drones = 5
	bin := build(t, "ingester", "alerter", "fakegen")
	natsURL := natstest.Start(t)
	pool, dbURL := dbtest.NewURL(t)
	udp := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), time.Minute)
	defer cancel()

	start(t, bin, "ingester", "-listen", udp, "-nats", natsURL, "-db", dbURL, "-metrics", "", "-interval", "1h")
	start(t, bin, "alerter", "-nats", natsURL, "-db", dbURL, "-metrics", "", "-low-battery", "101")
	time.Sleep(time.Second)

	nc, err := nats.Connect(natsURL)
	if err != nil {
		t.Fatal(err)
	}
	defer nc.Close()
	got := make(chan alert.Alert, 100)
	if _, err := nc.Subscribe(alert.Subjects, func(m *nats.Msg) {
		var a alert.Alert
		if json.Unmarshal(m.Data, &a) == nil {
			got <- a
		}
	}); err != nil {
		t.Fatal(err)
	}
	nc.Flush() //nolint:errcheck

	fleet := start(t, bin, "fakegen", "-count", strconv.Itoa(drones), "-target", udp)
	// Far from the fleet, which flies near ArduPilot's SITL home.
	data, _ := json.Marshal(alert.SetFence{User: "integration", Polygon: []alert.Point{{Lat: 10, Lon: 10}, {Lat: 10, Lon: 11}, {Lat: 11, Lon: 11}}})
	if _, err := nc.Request(alert.GeofenceSet, data, 2*time.Second); err != nil {
		t.Fatal(err)
	}
	raised := map[string]map[int64]int{} // kind → drone → times raised
	expect := func(kind string) {
		t.Helper()
		for len(raised[kind]) < drones {
			select {
			case a := <-got:
				if !a.Raised {
					continue
				}
				if raised[a.Kind] == nil {
					raised[a.Kind] = map[int64]int{}
				}
				raised[a.Kind][a.DroneID]++
			case <-ctx.Done():
				t.Fatalf("%s: %d of %d drones", kind, len(raised[kind]), drones)
			}
		}
	}
	expect(alert.LowBattery)
	expect(alert.Geofence)

	fleet.Process.Kill() //nolint:errcheck
	fleet.Wait()         //nolint:errcheck
	expect(alert.LinkLost)
	for kind, byDrone := range raised {
		for id, n := range byDrone {
			if n != 1 {
				t.Errorf("drone %d: %s raised %d times", id, kind, n)
			}
		}
	}

	var n int
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM events WHERE type = 'alert'`).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n < 3*drones {
		t.Errorf("%d events, want at least %d", n, 3*drones)
	}
}
