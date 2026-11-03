// Command gateway serves drone state to browsers (guide Phase 3): a WebSocket
// for live updates, a small REST API next to it (plan open item 3), and
// optionally the built web app.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/linkstats"
	"github.com/ChengCK18/murmur/internal/metrics"
)

type config struct {
	listen, natsURL, redisURL, metricsAddr, web string
	origins                                     []string
	tenant                                      int64
	throttle, timeout                           time.Duration
	maxSubs                                     int
}

func main() {
	var cfg config
	var origins string
	flag.StringVar(&cfg.listen, "listen", ":8080", "HTTP address for the WebSocket and REST API")
	flag.StringVar(&cfg.natsURL, "nats", "nats://127.0.0.1:4222", "NATS server")
	flag.StringVar(&cfg.redisURL, "redis", "127.0.0.1:6379", "Redis address, or a redis:// URL")
	flag.StringVar(&cfg.metricsAddr, "metrics", ":9103", `address to serve Prometheus /metrics on; "" for none`)
	flag.StringVar(&cfg.web, "web", "", "directory of the built web app to serve at /; \"\" for none")
	flag.StringVar(&origins, "origins", "localhost:5173,127.0.0.1:5173", "other origins allowed to open the WebSocket (the Vite dev server)")
	flag.Int64Var(&cfg.tenant, "tenant", 1, "tenant whose drones are served")
	flag.DurationVar(&cfg.throttle, "throttle", 250*time.Millisecond, "send each browser each drone at most this often")
	flag.DurationVar(&cfg.timeout, "timeout", linkstats.DefaultTimeout, "no heartbeat for this long: link lost")
	flag.IntVar(&cfg.maxSubs, "max-subs", 100, "most drones one browser can subscribe to")
	flag.Parse()
	log.SetFlags(log.Ltime)
	for _, o := range strings.Split(origins, ",") {
		if o = strings.TrimSpace(o); o != "" {
			cfg.origins = append(cfg.origins, o)
		}
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, cfg, nil); err != nil {
		log.Fatalf("gateway: %v", err)
	}
}

// run serves until ctx ends. If ready is not nil it receives the HTTP
// address once the gateway is listening (for tests).
func run(ctx context.Context, cfg config, ready chan<- string) error {
	opts := &redis.Options{Addr: cfg.redisURL}
	if strings.Contains(cfg.redisURL, "://") {
		var err error
		if opts, err = redis.ParseURL(cfg.redisURL); err != nil {
			return fmt.Errorf("redis: %w", err)
		}
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	nc, err := bus.Connect(cfg.natsURL, "murmur-gateway")
	if err != nil {
		return err
	}
	defer nc.Close()
	js, err := jetstream.New(nc)
	if err != nil {
		return err
	}
	setup, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	if _, err := bus.EnsureTelemetryStream(setup, js); err != nil {
		return err
	}

	h := newHub(cfg.tenant, cfg.timeout, cfg.maxSubs, rdb)
	s := &server{hub: h, nc: nc, throttle: cfg.throttle, origins: cfg.origins, web: cfg.web}
	if cfg.metricsAddr != "" {
		reg := metrics.NewRegistry()
		s.registerMetrics(reg)
		if err := metrics.Serve(ctx, cfg.metricsAddr, reg); err != nil {
			return err
		}
	}

	feedErr := make(chan error, 1)
	go func() { feedErr <- runFeed(ctx, js, h) }()
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				h.checkLinks(now)
			}
		}
	}()

	ln, err := net.Listen("tcp", cfg.listen)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: s.routes(), ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		srv.Shutdown(shutdown) //nolint:errcheck
	}()
	log.Printf("serving on %s (WebSocket /ws, REST /api), each drone at most every %v, %d drones per browser",
		ln.Addr(), cfg.throttle, cfg.maxSubs)
	if ready != nil {
		ready <- ln.Addr().String()
	}
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	select {
	case err := <-feedErr:
		return err
	case <-time.After(time.Second):
		return nil
	}
}
