//go:build integration

// Package integration runs Murmur's real binaries end to end (guide step
// 2.10, the Phase 2 check):
//
//	go test -tags integration -count=1 -v ./integration/
//
// It needs the Compose TimescaleDB and Redis (deploy/docker-compose.yml, or
// MURMUR_TEST_DATABASE_URL). Everything it writes is its own: a NATS server
// in the test process, a fresh database, and a spare Redis database.
package integration

import (
	"bufio"
	"context"
	"fmt"
	"math/rand/v2"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/bus/natstest"
	"github.com/ChengCK18/murmur/internal/db/dbtest"
)

const (
	drones   = 10
	messages = 20000
	downtime = 30 * time.Second
)

func TestPipelineSurvivesConsumerRestart(t *testing.T) {
	bin := build(t, "ingester", "consumer", "fakegen")
	natsURL := natstest.Start(t)
	pool, dbURL := dbtest.NewURL(t)
	redisURL, rdb := spareRedis(t)
	udp := freeUDPPort(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	consumerArgs := []string{"-nats", natsURL, "-db", dbURL, "-redis", redisURL, "-metrics", "", "-interval", "5s"}
	consumer := start(t, bin, "consumer", consumerArgs...)
	waitFor(t, ctx, "the consumer to create its durable consumer", func() bool {
		return durableExists(ctx, natsURL)
	})
	ingester := start(t, bin, "ingester", "-listen", udp, "-nats", natsURL, "-db", dbURL, "-metrics", "", "-interval", "1h")
	time.Sleep(time.Second) // listening, and the drones table created
	fakegen := start(t, bin, "fakegen", "-count", strconv.Itoa(drones), "-target", udp, "-messages", strconv.Itoa(messages))

	// 1. Kill the consumer a quarter of the way through: no graceful finish,
	// so a batch can be left written but unacknowledged.
	waitFor(t, ctx, "a quarter of the rows", func() bool { return rows(t, ctx, pool) >= messages/4 })
	if err := consumer.Process.Kill(); err != nil {
		t.Fatal(err)
	}
	consumer.Wait() //nolint:errcheck // killed
	atKill := rows(t, ctx, pool)
	t.Logf("consumer killed at %d rows; down for %v while the drones keep sending", atKill, downtime)

	// 2. Down for 30 s: nothing is written, the stream keeps everything.
	time.Sleep(downtime)
	if n := rows(t, ctx, pool); n != atKill {
		t.Fatalf("rows changed from %d to %d with no consumer running", atKill, n)
	}

	// 3. Start it again; once the drones have sent everything and the
	// consumer has caught up, every message is stored exactly once.
	consumer = start(t, bin, "consumer", consumerArgs...)
	if err := fakegen.Wait(); err != nil {
		t.Fatalf("fakegen: %v", err)
	}
	waitFor(t, ctx, "consumer lag 0", func() bool { return lag(ctx, natsURL) == 0 })

	if n := rows(t, ctx, pool); n != messages {
		t.Errorf("%d rows, want exactly %d", n, messages)
	}
	var dups int64
	if err := pool.QueryRow(ctx, `SELECT count(*) FROM (SELECT 1 FROM telemetry
		GROUP BY drone_id, msg_type, received_at HAVING count(*) > 1) d`).Scan(&dups); err != nil {
		t.Fatal(err)
	}
	if dups != 0 {
		t.Errorf("%d duplicate keys", dups)
	}
	var droneCount int64
	if err := pool.QueryRow(ctx, "SELECT count(DISTINCT drone_id) FROM telemetry").Scan(&droneCount); err != nil || droneCount != drones {
		t.Errorf("rows from %d drones (err %v), want %d", droneCount, err, drones)
	}
	if keys, err := rdb.Keys(ctx, "drone:1:*").Result(); err != nil || len(keys) != drones {
		t.Errorf("Redis has %d drone states (err %v), want %d", len(keys), err, drones)
	}

	// The ingester's own count agrees: every message published, none failed.
	ingester.Process.Signal(syscall.SIGINT) //nolint:errcheck
	ingester.Wait()                         //nolint:errcheck
	published, errs := lastReport(t, ingester)
	if published != messages || errs != 0 {
		t.Errorf("ingester published %d with %d errors, want %d and 0", published, errs, messages)
	}
	consumer.Process.Signal(syscall.SIGINT) //nolint:errcheck
	consumer.Wait()                         //nolint:errcheck
}

// build compiles the commands into a temporary directory.
func build(t *testing.T, cmds ...string) string {
	t.Helper()
	dir := t.TempDir()
	for _, c := range cmds {
		out, err := exec.Command("go", "build", "-o", filepath.Join(dir, c), "../cmd/"+c).CombinedOutput()
		if err != nil {
			t.Fatalf("build %s: %v\n%s", c, err, out)
		}
	}
	return dir
}

// start runs a command with its output in a file named after it, shown if
// the test fails.
func start(t *testing.T, bin, name string, args ...string) *exec.Cmd {
	t.Helper()
	log, err := os.CreateTemp(t.TempDir(), name+"-*.log")
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(filepath.Join(bin, name), args...)
	cmd.Stdout, cmd.Stderr = log, log
	if err := cmd.Start(); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if cmd.ProcessState == nil {
			cmd.Process.Kill() //nolint:errcheck
			cmd.Wait()         //nolint:errcheck
		}
		if t.Failed() {
			out, _ := os.ReadFile(log.Name())
			t.Logf("--- %s output (last 3 kB):\n%s", name, tail(out, 3000))
		}
	})
	return cmd
}

func tail(b []byte, n int) []byte {
	if len(b) > n {
		return b[len(b)-n:]
	}
	return b
}

// lastReport reads the ingester's final console header.
func lastReport(t *testing.T, cmd *exec.Cmd) (published, errs int) {
	t.Helper()
	f, err := os.Open(cmd.Stdout.(*os.File).Name())
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()
	re := regexp.MustCompile(`published (\d+)  publish errors (\d+)`)
	sc := bufio.NewScanner(f)
	found := false
	for sc.Scan() {
		if m := re.FindStringSubmatch(sc.Text()); m != nil {
			published, _ = strconv.Atoi(m[1])
			errs, _ = strconv.Atoi(m[2])
			found = true
		}
	}
	if !found {
		t.Fatal("no report from the ingester")
	}
	return published, errs
}

func rows(t *testing.T, ctx context.Context, pool *pgxpool.Pool) int64 {
	t.Helper()
	var n int64
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM telemetry").Scan(&n); err != nil {
		return -1 // the consumer may not have migrated yet
	}
	return n
}

func jetStream(ctx context.Context, url string) (jetstream.Consumer, func(), error) {
	nc, err := nats.Connect(url)
	if err != nil {
		return nil, nil, err
	}
	js, err := jetstream.New(nc)
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	c, err := js.Consumer(ctx, bus.TelemetryStream, "db-writer")
	if err != nil {
		nc.Close()
		return nil, nil, err
	}
	return c, nc.Close, nil
}

func durableExists(ctx context.Context, url string) bool {
	_, closeNC, err := jetStream(ctx, url)
	if err != nil {
		return false
	}
	closeNC()
	return true
}

// lag is messages pending plus delivered but unacknowledged, or -1.
func lag(ctx context.Context, url string) int64 {
	c, closeNC, err := jetStream(ctx, url)
	if err != nil {
		return -1
	}
	defer closeNC()
	info, err := c.Info(ctx)
	if err != nil {
		return -1
	}
	return int64(info.NumPending) + int64(info.NumAckPending)
}

func waitFor(t *testing.T, ctx context.Context, what string, cond func() bool) {
	t.Helper()
	for !cond() {
		select {
		case <-ctx.Done():
			t.Fatalf("timed out waiting for %s", what)
		case <-time.After(250 * time.Millisecond):
		}
	}
}

// spareRedis returns the URL of an empty Redis database (1 to 15), flushed
// again afterwards, or skips the test.
func spareRedis(t *testing.T) (string, *redis.Client) {
	t.Helper()
	n := 1 + rand.IntN(15)
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:6379", DB: n})
	ctx := context.Background()
	if err := rdb.Ping(ctx).Err(); err != nil {
		t.Skipf("no Redis at 127.0.0.1:6379 (start deploy/docker-compose.yml): %v", err)
	}
	rdb.FlushDB(ctx)
	t.Cleanup(func() { rdb.FlushDB(ctx); rdb.Close() })
	return fmt.Sprintf("redis://127.0.0.1:6379/%d", n), rdb
}

func freeUDPPort(t *testing.T) string {
	t.Helper()
	c, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	defer c.Close()
	return c.LocalAddr().String()
}
