package main

import (
	"context"
	"fmt"
	"math"
	"time"

	"github.com/ChengCK18/murmur/internal/state"
)

// bbox is a map area in degrees: west, south, east, north. East may be past
// 180 when the map is scrolled across the date line, as MapLibre reports it.
type bbox struct {
	west, south, east, north float64
}

func parseBBox(v []float64) (bbox, error) {
	if len(v) != 4 {
		return bbox{}, fmt.Errorf("bbox has %d numbers, want minLng, minLat, maxLng, maxLat", len(v))
	}
	b := bbox{west: v[0], south: v[1], east: v[2], north: v[3]}
	if b.west > b.east || b.south > b.north {
		return bbox{}, fmt.Errorf("bbox %v is empty", v)
	}
	return b, nil
}

// contains reports whether d's latest position is inside. A drone with no
// position yet is in no area.
func (b bbox) contains(d *state.Drone) bool {
	p := d.Position
	if p == nil || p.Lat < b.south || p.Lat > b.north {
		return false
	}
	east := math.Mod(p.Lon-b.west, 360) // degrees east of the west edge
	if east < 0 {
		east += 360
	}
	return east <= b.east-b.west
}

// subscribeArea replaces the client's map area (plan FR-12) and queues a
// snapshot of the drones newly inside it. Area subscriptions do not count
// towards maxSubs: a browser cannot know the IDs in a part of the map, and
// their updates are throttled harder (plan open item 4).
func (h *hub) subscribeArea(ctx context.Context, cl *client, b bbox) {
	h.loadOnce(ctx)
	h.mu.Lock()
	defer h.mu.Unlock()
	if cl.area == nil {
		h.areas[cl] = struct{}{}
		cl.inArea = map[int64]struct{}{}
	}
	cl.area = &b
	now := time.Now()
	var left []int64
	for id := range cl.inArea {
		if d := h.drones[id]; d == nil || !b.contains(d) {
			delete(cl.inArea, id)
			left = append(left, id)
		}
	}
	cl.forgetArea(left)
	var snapshot []*state.Drone
	for id, d := range h.drones {
		if _, known := cl.inArea[id]; known || !b.contains(d) {
			continue
		}
		if h.isStale(d, now) {
			h.removed[id] = true // never shown, so nothing to remove later
			continue
		}
		cl.inArea[id] = struct{}{}
		d.UpdateLink(now, h.timeout)
		snapshot = append(snapshot, d.Clone())
	}
	// Queued under h.mu: any later offer for these drones lands after it.
	cl.snapshot(snapshot)
}

// offerAreas gives d to the clients following an area it is in, and one last
// time to those it has just left, so its marker is not left standing inside
// the map. c is a shared copy of d. The caller holds h.mu.
func (h *hub) offerAreas(d *state.Drone, c *state.Drone) {
	subs := h.subs[d.DroneID]
	for cl := range h.areas {
		_, was := cl.inArea[d.DroneID]
		in := cl.area.contains(d)
		if in {
			cl.inArea[d.DroneID] = struct{}{}
		} else {
			delete(cl.inArea, d.DroneID)
		}
		if _, byID := subs[cl]; (in || was) && !byID {
			cl.offerArea(c)
		}
	}
}
