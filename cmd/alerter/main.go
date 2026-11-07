// Command alerter checks the fleet for low battery, lost links and geofence
// breaches (guide step 5.6). It reads TELEMETRY with its own durable consumer,
// so slow alert logic never holds up database writes (plan open item 5).
// Alerts are written to the events table and published on alert.<drone_id>.
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"log"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ChengCK18/murmur/internal/alert"
	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/db"
	"github.com/ChengCK18/murmur/internal/linkstats"
	"github.com/ChengCK18/murmur/internal/metrics"
	"github.com/ChengCK18/murmur/internal/schema"
)

func main() {
	natsURL := flag.String("nats", "nats://127.0.0.1:4222", "NATS server")
	dbURL := flag.String("db", "postgres://murmur:murmur@127.0.0.1:5432/murmur", "TimescaleDB")
	tenant := flag.Int64("tenant", 1, "tenant ID")
	lowBattery := flag.Int("low-battery", 20, "alert at or below this battery percentage")
	timeout := flag.Duration("timeout", linkstats.DefaultTimeout, "no heartbeat for this long: link lost")
	metricsAddr := flag.String("metrics", ":9105", `address to serve Prometheus /metrics on; "" for none`)
	flag.Parse()
	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *natsURL, *dbURL, *tenant, *lowBattery, *timeout, *metricsAddr); err != nil {
		log.Fatalf("alerter: %v", err)
	}
}

func run(ctx context.Context, natsURL, dbURL string, tenant int64, lowBattery int, timeout time.Duration, metricsAddr string) error {
	setup, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	pool, err := pgxpool.New(setup, dbURL)
	if err != nil {
		return err
	}
	defer pool.Close()
	if _, err := db.Migrate(setup, pool); err != nil {
		return err
	}
	st := store{pool: pool, tenant: tenant}

	nc, err := bus.Connect(natsURL, "murmur-alerter")
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
	// Its own durable, starting from now on first creation: alerts about the
	// past ten minutes would be news to nobody.
	cons, err := js.CreateOrUpdateConsumer(setup, bus.TelemetryStream, jetstream.ConsumerConfig{
		Durable: "alerter", FilterSubject: bus.TelemetrySubjects,
		AckPolicy: jetstream.AckAllPolicy, DeliverPolicy: jetstream.DeliverNewPolicy, MaxAckPending: 20000,
	})
	if err != nil {
		return err
	}

	var raised, cleared, writeErrors metricsCount
	r := newRules(lowBattery, timeout, 10*time.Minute, func(a alert.Alert) {
		if a.Raised {
			raised.inc()
		} else {
			cleared.inc()
		}
		wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := st.event(wctx, a); err != nil {
			writeErrors.inc()
			log.Print(err)
		}
		data, _ := json.Marshal(a)
		if err := nc.Publish(alert.Subject(a.DroneID), data); err != nil {
			log.Printf("publishing an alert: %v", err)
		}
		verb := "cleared"
		if a.Raised {
			verb = "raised"
		}
		log.Printf("drone %d: %s %s: %s", a.DroneID, a.Kind, verb, a.Message)
	})

	fence, err := st.fence(setup)
	if err != nil {
		return err
	}
	r.setFence(fence.Polygon, time.Now())

	if metricsAddr != "" {
		reg := metrics.NewRegistry()
		metrics.Counter(reg, "murmur_alerter_raised_total", "Alerts raised.", raised.get)
		metrics.Counter(reg, "murmur_alerter_cleared_total", "Alerts cleared.", cleared.get)
		metrics.Counter(reg, "murmur_alerter_event_errors_total", "Alerts not written to the events table.", writeErrors.get)
		if err := metrics.Serve(ctx, metricsAddr, reg); err != nil {
			return err
		}
	}

	if err := serve(ctx, nc, st, r); err != nil {
		return err
	}
	go func() {
		t := time.NewTicker(time.Second)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				r.tick(now)
			}
		}
	}()

	log.Printf("watching %s: battery at %d%% or below, no heartbeat for %v, geofence of %d points",
		bus.TelemetryStream, lowBattery, timeout, len(fence.Polygon))
	for ctx.Err() == nil {
		batch, err := cons.Fetch(500, jetstream.FetchMaxWait(250*time.Millisecond))
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("fetch: %v", err)
				time.Sleep(time.Second)
			}
			continue
		}
		var last jetstream.Msg
		for m := range batch.Messages() {
			var t schema.Telemetry
			if err := json.Unmarshal(m.Data(), &t); err == nil && t.TenantID == tenant {
				r.observe(t)
			}
			last = m
		}
		if err := batch.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) && ctx.Err() == nil {
			log.Printf("fetch: %v", err)
		}
		if last != nil {
			last.Ack() //nolint:errcheck // AckAll: covers the batch; a lost ack means a redelivery, which raises nothing new
		}
	}
	return nil
}

// serve answers the gateway: the alerts raised now, and the geofence.
func serve(ctx context.Context, nc *nats.Conn, st store, r *rules) error {
	reply := func(m *nats.Msg, v any) {
		data, _ := json.Marshal(v)
		if err := m.Respond(data); err != nil {
			log.Printf("answering on %s: %v", m.Subject, err)
		}
	}
	if _, err := nc.Subscribe(alert.ActiveSubject, func(m *nats.Msg) { reply(m, r.active()) }); err != nil {
		return err
	}
	if _, err := nc.Subscribe(alert.GeofenceGet, func(m *nats.Msg) {
		go func() {
			f, err := st.fence(ctx)
			if err != nil {
				log.Print(err)
				return
			}
			reply(m, f)
		}()
	}); err != nil {
		return err
	}
	_, err := nc.Subscribe(alert.GeofenceSet, func(m *nats.Msg) {
		go func() {
			var req alert.SetFence
			if err := json.Unmarshal(m.Data, &req); err != nil {
				reply(m, alert.SetResult{Error: "bad request"})
				return
			}
			if len(req.Polygon) != 0 && len(req.Polygon) < 3 {
				reply(m, alert.SetResult{Error: "a geofence needs at least 3 points"})
				return
			}
			if req.User == "" {
				reply(m, alert.SetResult{Error: "no user"})
				return
			}
			f := alert.Fence{Polygon: req.Polygon, UpdatedBy: req.User, UpdatedAt: time.Now()}
			if err := st.setFence(ctx, f); err != nil {
				log.Print(err)
				reply(m, alert.SetResult{Error: "could not save the geofence"})
				return
			}
			r.setFence(f.Polygon, f.UpdatedAt)
			reply(m, alert.SetResult{Fence: f})
			data, _ := json.Marshal(f)
			nc.Publish(alert.GeofenceChanged, data) //nolint:errcheck // browsers also reload it on reconnect
		}()
	})
	return err
}
