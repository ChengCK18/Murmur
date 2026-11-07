package main

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/ChengCK18/murmur/internal/db"
	"github.com/ChengCK18/murmur/internal/db/dbtest"
)

func TestHistory(t *testing.T) {
	pool := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := db.Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	t0 := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	insert := func(at time.Duration, msg string, lat, lon *float64, battery *int, payload map[string]any) {
		t.Helper()
		js, _ := json.Marshal(payload)
		if _, err := pool.Exec(ctx, `INSERT INTO telemetry (tenant_id, drone_id, msg_type, received_at, sys_id, comp_id, seq,
			lat, lon, alt_m, rel_alt_m, speed_ms, battery_pct, payload)
			VALUES (1, 7, $1, $2, 1, 1, 0, $3, $4, 600, 20, 5, $5, $6::jsonb)`, msg, t0.Add(at), lat, lon, battery, string(js)); err != nil {
			t.Fatal(err)
		}
	}
	f := func(v float64) *float64 { return &v }
	b := func(v int) *int { return &v }
	// 10 s of flight: positions at 5 Hz, a battery and heartbeat each second,
	// and a no-fix position that must not be drawn.
	for i := range 50 {
		at := time.Duration(i) * 200 * time.Millisecond
		insert(at, "GLOBAL_POSITION_INT", f(-35.36+float64(i)*1e-5), f(149.16), nil, map[string]any{})
		if i%5 == 0 {
			insert(at+time.Millisecond, "SYS_STATUS", nil, nil, b(90-i/5), map[string]any{})
			mode := 3
			if i >= 25 {
				mode = 6 // RTL
			}
			insert(at+2*time.Millisecond, "HEARTBEAT", nil, nil, nil, map[string]any{"custom_mode": mode, "base_mode": 217, "autopilot": 3})
		}
	}
	insert(10*time.Second, "GLOBAL_POSITION_INT", f(0), f(0), nil, map[string]any{})
	insert(11*time.Second, "HEARTBEAT", nil, nil, nil, map[string]any{"custom_mode": 0, "base_mode": 81, "autopilot": 8}) // a gimbal

	points, step, err := history(ctx, pool, 1, 7, t0, t0.Add(20*time.Second))
	if err != nil {
		t.Fatal(err)
	}
	if step != time.Second || len(points) != 10 {
		t.Fatalf("step %v, %d points", step, len(points))
	}
	first, last := points[0], points[9]
	if first.Mode != "AUTO" || !first.Armed || first.BatteryPct == nil || *first.BatteryPct != 90 {
		t.Errorf("first %+v", first)
	}
	if last.Mode != "RTL" || *last.BatteryPct != 81 || last.Lat <= first.Lat {
		t.Errorf("last %+v", last)
	}
	for _, p := range points {
		if p.Lat == 0 {
			t.Error("a no-fix position was returned")
		}
	}
	if other, _, _ := history(ctx, pool, 2, 7, t0, t0.Add(20*time.Second)); len(other) != 0 {
		t.Errorf("another tenant saw %d points", len(other))
	}
}

func TestHistoryStep(t *testing.T) {
	for span, want := range map[time.Duration]time.Duration{
		time.Minute:      time.Second,
		10 * time.Minute: time.Second,
		time.Hour:        time.Second,
		24 * time.Hour:   29 * time.Second,
	} {
		if got := historyStep(span); got != want {
			t.Errorf("%v: %v, want %v", span, got, want)
		}
	}
}

func TestHistoryRange(t *testing.T) {
	now := time.Date(2026, 11, 7, 10, 0, 0, 0, time.UTC)
	from, to, err := historyRange("", "", now)
	if err != nil || to != now || to.Sub(from) != 10*time.Minute {
		t.Errorf("default: %v to %v, err %v", from, to, err)
	}
	bad := [][2]string{
		{"yesterday", ""},
		{now.Format(time.RFC3339), now.Add(-time.Hour).Format(time.RFC3339)},
		{now.Add(-48 * time.Hour).Format(time.RFC3339), now.Format(time.RFC3339)},
	}
	for _, c := range bad {
		if _, _, err := historyRange(c[0], c[1], now); err == nil {
			t.Errorf("%v accepted", c)
		}
	}
}
