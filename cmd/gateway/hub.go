package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"slices"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/command"
	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/state"
)

// hub holds the latest state of every drone and who is subscribed to it.
type hub struct {
	tenant  int64
	timeout time.Duration // no heartbeat for this long: link lost
	stale   time.Duration // nothing heard for this long: hidden from browsers; 0 keeps every drone
	maxSubs int           // per browser (plan open item 4)
	rdb     *redis.Client

	mu        sync.Mutex
	drones    map[int64]*state.Drone // merged from the feed and Redis
	loaded    map[int64]bool         // Redis hash merged in already
	removed   map[int64]bool         // stale, and subscribers told so
	subs      map[int64]map[*client]struct{}
	areas     map[*client]struct{} // clients following a map area
	users     map[string]map[*client]struct{}
	clients   map[*client]struct{} // every browser, for alerts
	allLoaded bool                 // every Redis hash merged in once

	feedMessages atomic.Uint64
	feedErrors   atomic.Uint64
}

func newHub(tenant int64, timeout time.Duration, maxSubs int, rdb *redis.Client) *hub {
	return &hub{
		tenant: tenant, timeout: timeout, maxSubs: maxSubs, rdb: rdb,
		drones:  map[int64]*state.Drone{},
		loaded:  map[int64]bool{},
		removed: map[int64]bool{},
		subs:    map[int64]map[*client]struct{}{},
		areas:   map[*client]struct{}{},
		users:   map[string]map[*client]struct{}{},
		clients: map[*client]struct{}{},
	}
}

// apply merges one live message and offers the new state to subscribers.
func (h *hub) apply(t schema.Telemetry, now time.Time) {
	h.feedMessages.Add(1)
	if t.TenantID != h.tenant {
		return
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	d, ok := h.drones[t.DroneID]
	if !ok {
		d = state.New(t.TenantID, t.DroneID)
		h.drones[t.DroneID] = d
	}
	changed, err := d.Apply(t)
	if err != nil {
		h.feedErrors.Add(1)
		return
	}
	if d.UpdateLink(now, h.timeout) || changed {
		delete(h.removed, t.DroneID) // heard from again
		h.offer(d)
	}
}

// checkLinks marks drones lost whose heartbeats stopped, and removes drones
// silent for longer than stale. A silent drone sends nothing that would
// trigger either, so this runs on a timer.
func (h *hub) checkLinks(now time.Time) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for id, d := range h.drones {
		if h.isStale(d, now) {
			if !h.removed[id] {
				h.removed[id] = true
				for cl := range h.subs[id] {
					cl.offerRemove(id)
				}
				for cl := range h.areas {
					if _, in := cl.inArea[id]; in {
						delete(cl.inArea, id)
						if _, byID := h.subs[id][cl]; !byID {
							cl.offerRemove(id)
						}
					}
				}
			}
			continue
		}
		if d.UpdateLink(now, h.timeout) {
			h.offer(d)
		}
	}
}

// isStale reports whether d has been silent for longer than h.stale. Old
// drones stay in Redis for good; without this the map fills with drones from
// past runs that will never fly again.
func (h *hub) isStale(d *state.Drone, now time.Time) bool {
	return h.stale > 0 && now.Sub(d.LastSeen) > h.stale
}

// offer gives every subscriber of d one shared copy. The caller holds h.mu.
func (h *hub) offer(d *state.Drone) {
	subs := h.subs[d.DroneID]
	if len(subs) == 0 && len(h.areas) == 0 {
		return
	}
	c := d.Clone()
	for cl := range subs {
		cl.offer(c)
	}
	h.offerAreas(d, c)
}

// subscribe adds drones to a client, up to the limit, and queues a snapshot
// of those with any state. It returns how many were refused.
func (h *hub) subscribe(ctx context.Context, cl *client, ids []int64) (refused int) {
	// Drones not merged with Redis yet are loaded first, outside the lock.
	h.mu.Lock()
	var missing []int64
	for _, id := range ids {
		if !h.loaded[id] {
			missing = append(missing, id)
		}
	}
	h.mu.Unlock()
	fromRedis := h.load(ctx, missing)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.mergeLoaded(fromRedis)
	now := time.Now()
	var snapshot []*state.Drone
	for _, id := range ids {
		if _, already := cl.subs[id]; !already {
			if len(cl.subs) >= h.maxSubs {
				refused++
				continue
			}
			cl.subs[id] = struct{}{}
			if h.subs[id] == nil {
				h.subs[id] = map[*client]struct{}{}
			}
			h.subs[id][cl] = struct{}{}
		}
		if d, ok := h.drones[id]; ok {
			if h.isStale(d, now) {
				h.removed[id] = true // never shown, so nothing to remove later
				continue
			}
			d.UpdateLink(now, h.timeout)
			snapshot = append(snapshot, d.Clone())
		}
	}
	// Queued under h.mu: any later offer for these drones lands after it.
	cl.snapshot(snapshot)
	return refused
}

func (h *hub) unsubscribe(cl *client, ids []int64) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for _, id := range ids {
		delete(cl.subs, id)
		delete(h.subs[id], cl)
		if len(h.subs[id]) == 0 {
			delete(h.subs, id)
		}
	}
	cl.forget(ids)
}

// remove drops a disconnected client.
func (h *hub) remove(cl *client) {
	h.mu.Lock()
	ids := make([]int64, 0, len(cl.subs))
	for id := range cl.subs {
		ids = append(ids, id)
	}
	delete(h.areas, cl)
	delete(h.clients, cl)
	h.mu.Unlock()
	h.unsubscribe(cl, ids)
}

// mergeLoaded applies messages read from Redis onto the hub's states.
// Per-type timestamps in state.Apply keep whichever of the live and stored
// message is newer. The caller holds h.mu.
func (h *hub) mergeLoaded(loaded map[int64][]schema.Telemetry) {
	for id, msgs := range loaded {
		h.loaded[id] = true
		d, ok := h.drones[id]
		if !ok {
			d = state.New(h.tenant, id)
			h.drones[id] = d
		}
		for _, t := range msgs {
			if _, err := d.Apply(t); err != nil {
				log.Printf("redis: drone %d %s: %v", id, t.Msg, err)
			}
		}
	}
}

// load reads drones' Redis hashes in one round trip. Drones without a hash
// are left out; on errors they are loaded on a later request.
func (h *hub) load(ctx context.Context, ids []int64) map[int64][]schema.Telemetry {
	out := map[int64][]schema.Telemetry{}
	if len(ids) == 0 {
		return out
	}
	pipe := h.rdb.Pipeline()
	cmds := make([]*redis.MapStringStringCmd, len(ids))
	for i, id := range ids {
		cmds[i] = pipe.HGetAll(ctx, stateKey(h.tenant, id))
	}
	if _, err := pipe.Exec(ctx); err != nil && err != redis.Nil {
		log.Printf("redis: %v", err)
		return out
	}
	for i, id := range ids {
		hash := cmds[i].Val()
		if len(hash) == 0 {
			continue
		}
		msgs, err := state.ParseRedis(hash)
		if err != nil {
			log.Printf("redis: drone %d: %v", id, err)
			continue
		}
		out[id] = msgs
	}
	return out
}

// loadOnce merges every Redis hash into the hub the first time it is needed.
// After that the feed brings every drone that is flying; a browser's map area
// needs drones the gateway has not heard from since it started, too.
func (h *hub) loadOnce(ctx context.Context) {
	h.mu.Lock()
	done := h.allLoaded
	h.mu.Unlock()
	if done {
		return
	}
	if err := h.loadAll(ctx); err != nil {
		log.Print(err) // the live feed still works, and the next area tries again
		return
	}
	h.mu.Lock()
	h.allLoaded = true
	h.mu.Unlock()
}

// loadAll merges every Redis hash not merged yet into the hub.
func (h *hub) loadAll(ctx context.Context) error {
	var ids []int64
	prefix := "drone:" + strconv.FormatInt(h.tenant, 10) + ":"
	iter := h.rdb.Scan(ctx, 0, prefix+"*", 500).Iterator()
	for iter.Next(ctx) {
		id, err := strconv.ParseInt(strings.TrimPrefix(iter.Val(), prefix), 10, 64)
		if err == nil {
			ids = append(ids, id)
		}
	}
	if err := iter.Err(); err != nil {
		return fmt.Errorf("redis scan: %w", err)
	}
	h.mu.Lock()
	var missing []int64
	for _, id := range ids {
		if !h.loaded[id] {
			missing = append(missing, id)
		}
	}
	h.mu.Unlock()
	fromRedis := h.load(ctx, missing)

	h.mu.Lock()
	defer h.mu.Unlock()
	h.mergeLoaded(fromRedis)
	return nil
}

// all returns every drone with stored or live state that is not stale,
// ordered by ID.
func (h *hub) all(ctx context.Context) ([]*state.Drone, error) {
	if err := h.loadAll(ctx); err != nil {
		return nil, err
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	now := time.Now()
	out := make([]*state.Drone, 0, len(h.drones))
	for id, d := range h.drones {
		if h.isStale(d, now) {
			h.removed[id] = true
			continue
		}
		d.UpdateLink(now, h.timeout)
		out = append(out, d.Clone())
	}
	slices.SortFunc(out, func(a, b *state.Drone) int { return int(a.DroneID - b.DroneID) })
	return out, nil
}

func stateKey(tenant, drone int64) string {
	return "drone:" + strconv.FormatInt(tenant, 10) + ":" + strconv.FormatInt(drone, 10)
}

// Messages sent to browsers.
type (
	snapshotMsg struct {
		Type   string         `json:"type"` // "snapshot"
		Drones []*state.Drone `json:"drones"`
	}
	updateMsg struct {
		Type    string       `json:"type"` // "update"
		DroneID int64        `json:"drone_id"`
		State   *state.Drone `json:"state"`
	}
	removeMsg struct {
		Type    string `json:"type"` // "remove": stale, take it off the map
		DroneID int64  `json:"drone_id"`
	}
	errorMsg struct {
		Type    string `json:"type"` // "error"
		Message string `json:"message"`
	}
)

// Messages from browsers.
type request struct {
	Type   string    `json:"type"` // "subscribe", "unsubscribe", "subscribe_area", "command", "fleet_command" or "mission_upload"
	Drones []int64   `json:"drones,omitempty"`
	BBox   []float64 `json:"bbox,omitempty"` // minLng, minLat, maxLng, maxLat

	// A command.
	RequestID string         `json:"request_id,omitempty"`
	DroneID   int64          `json:"drone_id,omitempty"`
	Command   string         `json:"command,omitempty"`
	Params    command.Params `json:"params"`

	// A mission upload.
	Waypoints []command.Waypoint `json:"waypoints,omitempty"`
	AltitudeM float64            `json:"altitude_m,omitempty"`
}

func encode(v any) []byte {
	b, err := json.Marshal(v)
	if err != nil {
		panic(err) // only our own types are encoded
	}
	return b
}
