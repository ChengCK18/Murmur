package main

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/state"
)

// at is a position in whole degrees.
func at(lat, lon float64) message.Message {
	return &ardupilotmega.MessageGlobalPositionInt{Lat: int32(lat * 1e7), Lon: int32(lon * 1e7)}
}

// fix applies drone id's position: seconds decides which message is newer.
func fix(t *testing.T, h *hub, id int64, seconds int, lat, lon float64) {
	t.Helper()
	now := t0.Add(time.Duration(seconds) * time.Second)
	h.apply(tel(t, id, now, at(lat, lon)), now)
}

// droneIDs lists the drones a message is about: a snapshot's, or the one
// in an update or removal.
func droneIDs(m map[string]any) []int {
	if ds, ok := m["drones"].([]any); ok {
		ids := []int{}
		for _, d := range ds {
			ids = append(ids, int(d.(map[string]any)["drone_id"].(float64)))
		}
		return ids
	}
	return []int{int(m["drone_id"].(float64))}
}

func summary(msgs []map[string]any) string {
	var out []string
	for _, m := range msgs {
		out = append(out, fmt.Sprint(m["type"], droneIDs(m)))
	}
	return fmt.Sprint(out)
}

func TestBBoxContains(t *testing.T) {
	pos := func(lat, lon float64) *state.Drone {
		return &state.Drone{Position: &state.Position{Lat: lat, Lon: lon}}
	}
	box := bbox{west: 149, south: -36, east: 150, north: -35}
	across := bbox{west: 170, south: -10, east: 190, north: 10} // the date line
	for _, tt := range []struct {
		name string
		b    bbox
		d    *state.Drone
		want bool
	}{
		{"inside", box, pos(-35.5, 149.5), true},
		{"on the edge", box, pos(-35, 150), true},
		{"north of it", box, pos(-34.9, 149.5), false},
		{"south of it", box, pos(-36.1, 149.5), false},
		{"east of it", box, pos(-35.5, 150.1), false},
		{"west of it", box, pos(-35.5, 148.9), false},
		{"no position", box, &state.Drone{}, false},
		{"across the date line, east side", across, pos(0, -175), true},
		{"across the date line, west side", across, pos(0, 175), true},
		{"beside it", across, pos(0, 160), false},
		{"the whole world", bbox{west: -400, south: -90, east: 400, north: 90}, pos(10, 20), true},
	} {
		if got := tt.b.contains(tt.d); got != tt.want {
			t.Errorf("%s: %v", tt.name, got)
		}
	}
}

func TestParseBBox(t *testing.T) {
	if b, err := parseBBox([]float64{1, 2, 3, 4}); err != nil || b != (bbox{1, 2, 3, 4}) {
		t.Errorf("good box: %v, %v", b, err)
	}
	for _, v := range [][]float64{nil, {1, 2, 3}, {1, 2, 3, 4, 5}, {3, 2, 1, 4}, {1, 4, 3, 2}} {
		if _, err := parseBBox(v); err == nil {
			t.Errorf("%v accepted", v)
		}
	}
}

func TestAreaFollowsDronesInAndOut(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	h.allLoaded = true
	fix(t, h, 1, 0, 0.5, 0.5) // inside
	fix(t, h, 2, 0, 5, 5)     // outside
	rec := &recorder{}
	cl, _ := connect(t, h, time.Millisecond, rec.send)
	h.subscribeArea(context.Background(), cl, bbox{0, 0, 1, 1})
	settle := func() { time.Sleep(20 * time.Millisecond) }

	settle()
	fix(t, h, 2, 1, 6, 6) // still outside: nothing sent
	settle()
	fix(t, h, 2, 2, 0.6, 0.6) // flies in
	settle()
	fix(t, h, 1, 3, 2, 2) // flies out: one last update for its marker
	settle()
	fix(t, h, 1, 4, 3, 3) // gone: nothing
	settle()

	if got, want := summary(rec.messages()), "[snapshot[1] update[2] update[1]]"; got != want {
		t.Errorf("messages %s, want %s", got, want)
	}
}

func TestAreaMoves(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	h.allLoaded = true
	fix(t, h, 1, 0, 0.5, 0.5)
	fix(t, h, 2, 0, 1.5, 0.5)
	fix(t, h, 3, 0, 2.5, 0.5)
	rec := &recorder{}
	cl, _ := connect(t, h, time.Millisecond, rec.send)
	ctx := context.Background()
	h.subscribeArea(ctx, cl, bbox{0, 0, 1, 2}) // drones 1 and 2
	time.Sleep(10 * time.Millisecond)
	h.subscribeArea(ctx, cl, bbox{0, 1, 1, 3}) // drones 2 and 3: only 3 is news
	time.Sleep(10 * time.Millisecond)
	fix(t, h, 1, 1, 0.6, 0.5) // no longer followed
	fix(t, h, 3, 1, 2.6, 0.5)
	time.Sleep(20 * time.Millisecond)

	msgs := rec.messages()
	if len(msgs) != 3 || fmt.Sprint(droneIDs(msgs[1])) != "[3]" || msgs[1]["type"] != "snapshot" {
		t.Fatalf("messages %s", summary(msgs))
	}
	if ids := droneIDs(msgs[2]); msgs[2]["type"] != "update" || ids[0] != 3 {
		t.Errorf("last message %s, want an update for drone 3", summary(msgs[2:]))
	}
	if ids := fmt.Sprint(len(cl.inArea)); ids != "2" {
		t.Errorf("%s drones in the area, want 2", ids)
	}
}

func TestAreaThrottledHarderAndNotLimited(t *testing.T) {
	// Five drones under a limit of two: an area is not limited, and its
	// updates go out four times less often than a subscribed drone's.
	h := newHub(1, 3*time.Second, 2, noRedis())
	h.allLoaded = true
	for id := int64(1); id <= 5; id++ {
		fix(t, h, id, 0, 0.5, 0.5)
	}
	rec := &recorder{}
	cl, _ := connect(t, h, 20*time.Millisecond, rec.send)
	ctx := context.Background()
	h.subscribeArea(ctx, cl, bbox{0, 0, 1, 1})
	h.subscribe(ctx, cl, []int64{1}) // also in the area: sent once, as a subscription
	for s := 1; s <= 400; s++ {
		for id := int64(1); id <= 5; id++ {
			fix(t, h, id, s, 0.5, 0.5+float64(s)/1e4)
		}
		time.Sleep(time.Millisecond)
	}
	time.Sleep(100 * time.Millisecond)

	count := map[int]int{}
	for _, m := range rec.messages() {
		if m["type"] == "update" {
			count[droneIDs(m)[0]]++
		}
	}
	for id := 2; id <= 5; id++ {
		if count[id] == 0 || count[1] < 3*count[id] {
			t.Errorf("drone %d: %d updates, subscribed drone %d: want about 4 times fewer", id, count[id], count[1])
		}
	}
}

func TestAreaDroneRemovedWhenStale(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	h.allLoaded = true
	h.stale = time.Minute
	now := time.Now()
	h.apply(tel(t, 1, now, at(0.5, 0.5)), now)
	rec := &recorder{}
	cl, _ := connect(t, h, time.Millisecond, rec.send)
	h.subscribeArea(context.Background(), cl, bbox{0, 0, 1, 1})
	h.checkLinks(now.Add(2 * time.Minute))
	time.Sleep(20 * time.Millisecond)
	h.checkLinks(now.Add(3 * time.Minute)) // once only
	time.Sleep(20 * time.Millisecond)
	if got, want := summary(rec.messages()), "[snapshot[1] remove[1]]"; got != want {
		t.Errorf("messages %s, want %s", got, want)
	}
	if len(cl.inArea) != 0 {
		t.Errorf("drones left in the area: %v", cl.inArea)
	}
}

func TestDisconnectedClientLeavesAreas(t *testing.T) {
	h := newHub(1, 3*time.Second, 100, noRedis())
	h.allLoaded = true
	rec := &recorder{}
	cl, _ := connect(t, h, time.Millisecond, rec.send)
	h.subscribeArea(context.Background(), cl, bbox{0, 0, 1, 1})
	h.remove(cl)
	if len(h.areas) != 0 {
		t.Errorf("%d areas after the browser left", len(h.areas))
	}
}
