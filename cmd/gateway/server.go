package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"net/http"
	"os"
	"path/filepath"
	"sync/atomic"
	"time"

	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/schema"
)

// writeTimeout closes a browser that cannot take a message for this long.
const writeTimeout = 5 * time.Second

type server struct {
	hub      *hub
	nc       *nats.Conn
	throttle time.Duration
	origins  []string // allowed WebSocket origins besides the gateway's own
	web      string   // built web app to serve, or ""
	counts   counters
	clients  atomic.Int64
}

func (s *server) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /ws", s.handleWS)
	mux.HandleFunc("GET /api/drones", s.handleDrones)
	mux.HandleFunc("GET /api/health", s.handleHealth)
	if s.web != "" {
		mux.Handle("GET /", spa(s.web))
	}
	return mux
}

// handleWS serves one browser (guide step 3.1):
//
//	browser → gateway  {"type":"subscribe","drones":[1,2,3]}  {"type":"unsubscribe","drones":[2]}
//	                   {"type":"subscribe_area","bbox":[minLng,minLat,maxLng,maxLat]}  replaces the last area
//	gateway → browser  {"type":"snapshot","drones":[...]}      on each subscribe; for an area, drones newly in it
//	                   {"type":"update","drone_id":1,"state":{...}}
//	                   {"type":"remove","drone_id":1}            silent for -stale
//	                   {"type":"error","message":"..."}
func (s *server) handleWS(w http.ResponseWriter, r *http.Request) {
	conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{OriginPatterns: s.origins})
	if err != nil {
		return // Accept has written the HTTP error
	}
	defer conn.CloseNow() //nolint:errcheck
	conn.SetReadLimit(64 << 10)

	ctx, cancel := context.WithCancel(r.Context())
	defer cancel()
	cl := newClient(func(ctx context.Context, b []byte) error {
		ctx, cancel := context.WithTimeout(ctx, writeTimeout)
		defer cancel()
		return conn.Write(ctx, websocket.MessageText, b)
	}, s.throttle, &s.counts)
	s.clients.Add(1)
	defer s.clients.Add(-1)
	defer s.hub.remove(cl)

	go func() {
		// A write that fails or takes longer than writeTimeout ends the
		// connection; the browser reconnects and gets a fresh snapshot.
		if err := cl.run(ctx); err != nil && ctx.Err() == nil {
			log.Printf("ws %s: %v", r.RemoteAddr, err)
		}
		cancel()
	}()

	for {
		var req request
		if err := wsjson.Read(ctx, conn, &req); err != nil {
			return
		}
		switch req.Type {
		case "subscribe":
			if refused := s.hub.subscribe(ctx, cl, req.Drones); refused > 0 {
				cl.errorf("subscription limit is %d drones; %d not subscribed", s.hub.maxSubs, refused)
			}
		case "unsubscribe":
			s.hub.unsubscribe(cl, req.Drones)
		case "subscribe_area":
			b, err := parseBBox(req.BBox)
			if err != nil {
				cl.errorf("subscribe_area: %v", err)
				continue
			}
			s.hub.subscribeArea(ctx, cl, b)
		default:
			cl.errorf("unknown request type %q", req.Type)
		}
	}
}

// handleDrones serves GET /api/drones: every drone with its latest state
// (guide step 3.4).
func (s *server) handleDrones(w http.ResponseWriter, r *http.Request) {
	drones, err := s.hub.all(r.Context())
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"drones": drones})
}

// handleHealth serves GET /api/health: 200 when NATS and Redis are reachable.
func (s *server) handleHealth(w http.ResponseWriter, r *http.Request) {
	ctx, cancel := context.WithTimeout(r.Context(), 2*time.Second)
	defer cancel()
	status, code := map[string]string{"status": "ok", "nats": "ok", "redis": "ok"}, http.StatusOK
	if !s.nc.IsConnected() {
		status["nats"], status["status"], code = s.nc.Status().String(), "degraded", http.StatusServiceUnavailable
	}
	if err := s.hub.rdb.Ping(ctx).Err(); err != nil {
		status["redis"], status["status"], code = err.Error(), "degraded", http.StatusServiceUnavailable
	}
	writeJSON(w, code, status)
}

func writeJSON(w http.ResponseWriter, code int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(code)
	json.NewEncoder(w).Encode(v) //nolint:errcheck
}

// spa serves a built single-page app: files that exist, and index.html for
// other page paths. A missing file with an extension (a script, a stylesheet)
// is a 404, not the page: HTML served as a script fails with an error that
// hides the real cause.
func spa(dir string) http.Handler {
	files := http.FileServer(http.Dir(dir))
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		name := filepath.Clean(r.URL.Path)
		if _, err := os.Stat(filepath.Join(dir, name)); errors.Is(err, os.ErrNotExist) && filepath.Ext(name) == "" {
			http.ServeFile(w, r, filepath.Join(dir, "index.html"))
			return
		}
		files.ServeHTTP(w, r)
	})
}

// runFeed reads new telemetry into the hub (guide step 3.2). An ordered
// consumer starting at new messages: the gateway needs no history, Redis
// has the latest state. It recreates itself if NATS restarts.
func runFeed(ctx context.Context, js jetstream.JetStream, h *hub) error {
	cons, err := js.OrderedConsumer(ctx, bus.TelemetryStream, jetstream.OrderedConsumerConfig{
		FilterSubjects: []string{bus.TelemetrySubjects},
		DeliverPolicy:  jetstream.DeliverNewPolicy,
	})
	if err != nil {
		return err
	}
	cc, err := cons.Consume(func(m jetstream.Msg) {
		var t schema.Telemetry
		if err := json.Unmarshal(m.Data(), &t); err != nil {
			h.feedErrors.Add(1)
			return
		}
		h.apply(t, time.Now())
	})
	if err != nil {
		return err
	}
	<-ctx.Done()
	cc.Stop()
	return nil
}
