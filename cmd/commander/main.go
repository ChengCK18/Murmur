// Command commander owns the command lifecycle (plan §4; guide step 5.1). It
// checks each request, records it, sends it through the ingester with retries,
// matches the drone's answer and reports every status, so that no command is
// left pending and none runs late.
package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"log"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/command"
	"github.com/ChengCK18/murmur/internal/db"
	"github.com/ChengCK18/murmur/internal/metrics"
)

func main() {
	natsURL := flag.String("nats", "nats://127.0.0.1:4222", "NATS server")
	dbURL := flag.String("db", "postgres://murmur:murmur@127.0.0.1:5432/murmur", "TimescaleDB")
	redisAddr := flag.String("redis", "127.0.0.1:6379", "Redis address, or a redis:// URL, holding each drone's latest state")
	tenant := flag.Int64("tenant", 1, "tenant ID")
	metricsAddr := flag.String("metrics", ":9104", `address to serve Prometheus /metrics on; "" for none`)
	flag.Parse()
	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if err := run(ctx, *natsURL, *dbURL, *redisAddr, *tenant, *metricsAddr); err != nil {
		log.Fatalf("commander: %v", err)
	}
}

func run(ctx context.Context, natsURL, dbURL, redisAddr string, tenant int64, metricsAddr string) error {
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
	st := pgStore{pool: pool, tenant: tenant}
	unknown, err := st.markUnknown(setup)
	if err != nil {
		return err
	}

	opts := &redis.Options{Addr: redisAddr}
	if strings.Contains(redisAddr, "://") {
		if opts, err = redis.ParseURL(redisAddr); err != nil {
			return fmt.Errorf("redis: %w", err)
		}
	}
	rdb := redis.NewClient(opts)
	defer rdb.Close()

	nc, err := bus.Connect(natsURL, "murmur-commander")
	if err != nil {
		return err
	}
	defer nc.Close()

	s := newService(nc.Publish, st, planTiming)
	s.check = checker{rdb: rdb, tenant: tenant, store: st}.check
	s.missions = st
	for _, u := range unknown {
		s.tell(u)
	}
	if len(unknown) > 0 {
		log.Printf("%d commands from before this start marked %s", len(unknown), command.Unknown)
	}
	if metricsAddr != "" {
		reg := metrics.NewRegistry()
		finals := prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "murmur_commander_commands_total", Help: "Commands by final status.",
		}, []string{"status"})
		reg.MustRegister(finals)
		s.obs = func(status string) { finals.WithLabelValues(status).Inc() }
		if err := metrics.Serve(ctx, metricsAddr, reg); err != nil {
			return err
		}
	}

	if _, err := nc.Subscribe(command.ReplySubjects, func(m *nats.Msg) {
		var r command.Reply
		if err := json.Unmarshal(m.Data, &r); err != nil {
			log.Printf("bad reply: %v", err)
			return
		}
		s.reply(r)
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(command.RequestSubject, func(m *nats.Msg) {
		go answer(ctx, s, m) // the audit write must not hold up the next request
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(command.FleetSubject, func(m *nats.Msg) {
		var req command.FleetRequest
		started := command.FleetStarted{Error: "bad request"}
		if err := json.Unmarshal(m.Data, &req); err == nil {
			started = s.submitFleet(ctx, req)
		}
		data, err := json.Marshal(started)
		if err == nil {
			err = m.Respond(data)
		}
		if err != nil {
			log.Printf("answering a fleet request: %v", err)
		}
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(command.MissionSubject, func(m *nats.Msg) {
		go func() {
			var req command.MissionRequest
			u := command.Update{Status: command.Rejected, Reason: "bad request"}
			if err := json.Unmarshal(m.Data, &req); err == nil {
				u = s.submitMission(ctx, req)
			}
			respond(m, u)
		}()
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(command.MissionGetSubject, func(m *nats.Msg) {
		go func() {
			var req command.MissionGet
			if err := json.Unmarshal(m.Data, &req); err != nil {
				log.Printf("bad mission request: %v", err)
				return
			}
			v, err := st.mission(ctx, req.DroneID)
			if err != nil {
				log.Print(err)
				return // the gateway's request times out and it reports that
			}
			respond(m, v)
		}()
	}); err != nil {
		return err
	}
	if _, err := nc.Subscribe(command.RecentSubject, func(m *nats.Msg) {
		go recent(ctx, st, m)
	}); err != nil {
		return err
	}
	log.Printf("taking commands on %s", command.RequestSubject)
	<-ctx.Done()
	s.wg.Wait()
	return nil
}

func answer(ctx context.Context, s *service, m *nats.Msg) {
	var req command.Request
	u := command.Update{Status: command.Rejected, Reason: "bad request"}
	if err := json.Unmarshal(m.Data, &req); err == nil {
		u = s.submit(ctx, req)
	}
	data, err := json.Marshal(u)
	if err == nil {
		err = m.Respond(data)
	}
	if err != nil {
		log.Printf("answering a request: %v", err)
	}
}

func recent(ctx context.Context, st pgStore, m *nats.Msg) {
	var req command.RecentRequest
	if err := json.Unmarshal(m.Data, &req); err != nil {
		log.Printf("bad request for recent commands: %v", err)
		return
	}
	var us []command.Update
	var err error
	if req.FleetID != "" {
		us, err = st.fleet(ctx, req.FleetID)
	} else {
		us, err = st.recent(ctx, req.DroneID, min(max(req.Limit, 1), 100))
	}
	if err != nil {
		log.Print(err)
		return // the gateway's request times out and it reports that
	}
	data, err := json.Marshal(us)
	if err == nil {
		err = m.Respond(data)
	}
	if err != nil {
		log.Printf("answering for recent commands: %v", err)
	}
}

func respond(m *nats.Msg, v any) {
	data, err := json.Marshal(v)
	if err == nil {
		err = m.Respond(data)
	}
	if err != nil {
		log.Printf("answering on %s: %v", m.Subject, err)
	}
}
