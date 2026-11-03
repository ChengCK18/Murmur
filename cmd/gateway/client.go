package main

import (
	"context"
	"fmt"
	"slices"
	"sync"
	"sync/atomic"
	"time"

	"github.com/ChengCK18/murmur/internal/state"
)

// counters are shared by all clients, for metrics.
type counters struct {
	sent      atomic.Uint64 // messages written to browsers
	coalesced atomic.Uint64 // updates replaced by a newer one before being sent
}

// client is one browser connection (guide step 3.3). The hub offers it states
// without ever waiting: each drone has one pending slot holding only the
// newest state, and the writer sends what is pending every throttle period.
// A burst of updates for a drone becomes one message, and a slow browser
// delays only itself while its slots keep being overwritten.
type client struct {
	send     func(context.Context, []byte) error
	throttle time.Duration
	counts   *counters

	subs map[int64]struct{} // guarded by hub.mu

	mu      sync.Mutex
	queue   [][]byte               // snapshots and errors, sent in order before updates
	pending map[int64]*state.Drone // nil: the drone was removed
	wake    chan struct{}
}

func newClient(send func(context.Context, []byte) error, throttle time.Duration, counts *counters) *client {
	return &client{
		send: send, throttle: throttle, counts: counts,
		subs:    map[int64]struct{}{},
		pending: map[int64]*state.Drone{},
		wake:    make(chan struct{}, 1),
	}
}

// offer replaces the drone's pending state. It never blocks.
func (c *client) offer(d *state.Drone) {
	c.mu.Lock()
	if _, ok := c.pending[d.DroneID]; ok {
		c.counts.coalesced.Add(1)
	}
	c.pending[d.DroneID] = d
	c.mu.Unlock()
}

// offerRemove replaces the drone's pending state with its removal, so a
// removal never overtakes an update sent before it.
func (c *client) offerRemove(id int64) {
	c.mu.Lock()
	c.pending[id] = nil
	c.mu.Unlock()
}

// snapshot queues the states of newly subscribed drones. Pending updates for
// them are dropped: the snapshot is at least as new.
func (c *client) snapshot(ds []*state.Drone) {
	c.mu.Lock()
	for _, d := range ds {
		delete(c.pending, d.DroneID)
	}
	if ds == nil {
		ds = []*state.Drone{}
	}
	c.queue = append(c.queue, encode(snapshotMsg{Type: "snapshot", Drones: ds}))
	c.mu.Unlock()
	c.poke()
}

func (c *client) errorf(format string, args ...any) {
	c.mu.Lock()
	c.queue = append(c.queue, encode(errorMsg{Type: "error", Message: fmt.Sprintf(format, args...)}))
	c.mu.Unlock()
	c.poke()
}

// forget drops pending updates for unsubscribed drones.
func (c *client) forget(ids []int64) {
	c.mu.Lock()
	for _, id := range ids {
		delete(c.pending, id)
	}
	c.mu.Unlock()
}

func (c *client) poke() {
	select {
	case c.wake <- struct{}{}:
	default:
	}
}

// run writes until ctx ends or a write fails. Queued messages go out at
// once; updates only on the throttle tick, so each drone is sent at most once
// per period.
func (c *client) run(ctx context.Context) error {
	t := time.NewTicker(c.throttle)
	defer t.Stop()
	for {
		var updates bool
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-c.wake:
		case <-t.C:
			updates = true
		}
		if err := c.flush(ctx, updates); err != nil {
			return err
		}
	}
}

func (c *client) flush(ctx context.Context, updates bool) error {
	c.mu.Lock()
	queue := c.queue
	c.queue = nil
	var pending map[int64]*state.Drone
	if updates && len(c.pending) > 0 {
		pending = c.pending
		c.pending = map[int64]*state.Drone{}
	}
	c.mu.Unlock()

	for _, b := range queue {
		if err := c.write(ctx, b); err != nil {
			return err
		}
	}
	ids := make([]int64, 0, len(pending))
	for id := range pending {
		ids = append(ids, id)
	}
	slices.Sort(ids)
	for _, id := range ids {
		msg := any(updateMsg{Type: "update", DroneID: id, State: pending[id]})
		if pending[id] == nil {
			msg = removeMsg{Type: "remove", DroneID: id}
		}
		if err := c.write(ctx, encode(msg)); err != nil {
			return err
		}
	}
	return nil
}

func (c *client) write(ctx context.Context, b []byte) error {
	if err := c.send(ctx, b); err != nil {
		return err
	}
	c.counts.sent.Add(1)
	return nil
}
