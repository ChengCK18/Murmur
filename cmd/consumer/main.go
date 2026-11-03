// Command consumer writes telemetry from the TELEMETRY JetStream stream to
// TimescaleDB, and each drone's latest state to Redis (guide step 2.6).
//
// It reads with a durable pull consumer, so after a restart it carries on
// from the last acknowledged message, and acknowledges a batch only after the
// database commit: nothing is lost across a restart, and the database's
// unique key drops anything delivered twice.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/db"
	"github.com/ChengCK18/murmur/internal/metrics"
)

func main() {
	natsURL := flag.String("nats", "nats://127.0.0.1:4222", "NATS server")
	dbURL := flag.String("db", "postgres://murmur:murmur@127.0.0.1:5432/murmur", "TimescaleDB")
	redisAddr := flag.String("redis", "127.0.0.1:6379", "Redis address, or a redis:// URL to choose a database")
	durable := flag.String("durable", "db-writer", "durable consumer name")
	batch := flag.Int("batch", 500, "most messages per batch")
	wait := flag.Duration("wait", 100*time.Millisecond, "longest wait to fill a batch")
	interval := flag.Duration("interval", 5*time.Second, "how often to print progress")
	metricsAddr := flag.String("metrics", ":9102", `address to serve Prometheus /metrics on; "" for none`)
	flag.Parse()
	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *natsURL, *dbURL, *redisAddr, *durable, *batch, *wait, *interval, *metricsAddr); err != nil {
		log.Fatalf("consumer: %v", err)
	}
}

func run(ctx context.Context, natsURL, dbURL, redisAddr, durable string, batch int, wait, interval time.Duration, metricsAddr string) error {
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()

	pool, err := pgxpool.New(setup, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	applied, err := db.Migrate(setup, pool)
	if err != nil {
		return err
	}
	for _, m := range applied {
		log.Printf("applied migration %s", m)
	}

	opts := &redis.Options{Addr: redisAddr}
	if strings.Contains(redisAddr, "://") {
		if opts, err = redis.ParseURL(redisAddr); err != nil {
			return fmt.Errorf("redis: %w", err)
		}
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()
	if err := rdb.Ping(setup).Err(); err != nil {
		return fmt.Errorf("redis %s: %w", redisAddr, err)
	}

	nc, err := bus.Connect(natsURL, "murmur-consumer")
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	if _, err := bus.EnsureTelemetryStream(setup, js); err != nil {
		return err
	}
	cons, err := js.CreateOrUpdateConsumer(setup, bus.TelemetryStream, consumerConfig(durable, batch))
	if err != nil {
		return fmt.Errorf("consumer %s: %w", durable, err)
	}

	c := &consumer{cons: cons, pool: pool, rdb: rdb, batch: batch, wait: wait}
	if metricsAddr != "" {
		reg := metrics.NewRegistry()
		c.obs = newObservers(reg, c)
		go c.obs.trackLag(ctx, c, time.Second)
		if err := metrics.Serve(ctx, metricsAddr, reg); err != nil {
			return err
		}
		log.Printf("metrics on %s/metrics", metricsAddr)
	}
	go report(ctx, c, interval)
	log.Printf("consuming %s as %q, batches of up to %d or %v", bus.TelemetryStream, durable, batch, wait)
	c.run(ctx)
	log.Printf("stopped: %s", c.stats.String())
	return nil
}

func report(ctx context.Context, c *consumer, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			lag := "?"
			if info, err := c.cons.Info(ctx); err == nil {
				lag = fmt.Sprint(info.NumPending + uint64(info.NumAckPending))
			}
			log.Printf("%s  lag %s", c.stats.String(), lag)
		}
	}
}

func (s *stats) String() string {
	return fmt.Sprintf("fetched %d  inserted %d  duplicates %d  rejected %d  failed batches %d",
		s.fetched.Load(), s.inserted.Load(), s.duplicates.Load(), s.rejected.Load(), s.failed.Load())
}
