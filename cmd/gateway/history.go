package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ChengCK18/murmur/internal/mavlink"
)

const (
	// maxHistory is the longest range one replay request may cover.
	maxHistory = 24 * time.Hour
	// historyPoints is about how many points a replay gets, however long its
	// range: 5 positions a second for an hour would otherwise be 18,000.
	historyPoints = 3000
	minStep       = time.Second
)

// point is one moment of a flight, from the typed columns (plan §7), so
// replay reads no JSON except the heartbeat's mode.
type point struct {
	T          time.Time `json:"t"`
	Lat        float64   `json:"lat"`
	Lon        float64   `json:"lon"`
	AltM       float32   `json:"alt_m"`
	RelAltM    float32   `json:"rel_alt_m"`
	SpeedMS    float32   `json:"speed_ms"`
	HeadingDeg *float32  `json:"heading_deg"`
	BatteryPct *int16    `json:"battery_pct"`
	Mode       string    `json:"mode,omitempty"`
	Armed      bool      `json:"armed"`
}

// historyStep is the bucket width for a range.
func historyStep(span time.Duration) time.Duration {
	return max(minStep, (span / historyPoints).Round(time.Second))
}

// Each series is bucketed on its own and joined on the bucket; the battery
// and mode, sent once a second, are carried forward over buckets without one.
const historyQuery = `
WITH pos AS (
	SELECT time_bucket($4::interval, received_at) AS t,
		last(lat, received_at) AS lat, last(lon, received_at) AS lon,
		last(alt_m, received_at) AS alt, last(rel_alt_m, received_at) AS rel,
		last(speed_ms, received_at) AS speed, last(heading_deg, received_at) AS hdg
	FROM telemetry
	WHERE tenant_id = $1 AND drone_id = $2 AND msg_type = 'GLOBAL_POSITION_INT'
		AND received_at >= $3 AND received_at < $5 AND NOT (lat = 0 AND lon = 0)
	GROUP BY 1
), bat AS (
	SELECT time_bucket($4::interval, received_at) AS t, last(battery_pct, received_at) AS battery
	FROM telemetry
	WHERE tenant_id = $1 AND drone_id = $2 AND msg_type = 'SYS_STATUS' AND received_at >= $3 AND received_at < $5
	GROUP BY 1
), hb AS (
	SELECT time_bucket($4::interval, received_at) AS t,
		last((payload->>'custom_mode')::bigint, received_at) AS mode,
		last((payload->>'base_mode')::int, received_at) AS base,
		last((payload->>'autopilot')::int, received_at) AS autopilot
	FROM telemetry
	WHERE tenant_id = $1 AND drone_id = $2 AND msg_type = 'HEARTBEAT' AND received_at >= $3 AND received_at < $5
		AND (payload->>'autopilot')::int <> 8 -- MAV_AUTOPILOT_INVALID: a camera or gimbal, not the flight controller
	GROUP BY 1
)
SELECT pos.t, pos.lat, pos.lon, pos.alt, pos.rel, pos.speed, pos.hdg, bat.battery, hb.mode, hb.base, hb.autopilot
FROM pos LEFT JOIN bat USING (t) LEFT JOIN hb USING (t)
ORDER BY pos.t`

// history reads a drone's flight between from and to.
func history(ctx context.Context, pool *pgxpool.Pool, tenant, drone int64, from, to time.Time) ([]point, time.Duration, error) {
	step := historyStep(to.Sub(from))
	rows, err := pool.Query(ctx, historyQuery, tenant, drone, from, step.String(), to)
	if err != nil {
		return nil, 0, fmt.Errorf("history: %w", err)
	}
	defer rows.Close()
	points := []point{}
	var battery *int16
	mode, armed := "", false
	for rows.Next() {
		var p point
		var bat *int16
		var custom *int64
		var base, autopilot *int32
		if err := rows.Scan(&p.T, &p.Lat, &p.Lon, &p.AltM, &p.RelAltM, &p.SpeedMS, &p.HeadingDeg, &bat, &custom, &base, &autopilot); err != nil {
			return nil, 0, fmt.Errorf("history: %w", err)
		}
		if bat != nil {
			battery = bat
		}
		if custom != nil && base != nil && autopilot != nil {
			hb := &ardupilotmega.MessageHeartbeat{CustomMode: uint32(*custom), BaseMode: ardupilotmega.MAV_MODE_FLAG(*base),
				Autopilot: ardupilotmega.MAV_AUTOPILOT(*autopilot)}
			mode, armed = mavlink.ModeName(hb), mavlink.IsArmed(hb)
		}
		p.BatteryPct, p.Mode, p.Armed = battery, mode, armed
		points = append(points, p)
	}
	return points, step, rows.Err()
}

// handleHistory serves GET /api/drones/{id}/history?from=&to= (RFC 3339; to
// defaults to now, from to 10 minutes before it): a drone's flight, for
// replay (guide step 6.1).
func (s *server) handleHistory(w http.ResponseWriter, r *http.Request) {
	if s.pool == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "replay needs the gateway's -db"})
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad drone ID"})
		return
	}
	from, to, err := historyRange(r.URL.Query().Get("from"), r.URL.Query().Get("to"), time.Now())
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 15*time.Second)
	defer cancel()
	points, step, err := history(ctx, s.pool, s.hub.tenant, id, from, to)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the database is not answering"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"drone_id": id, "from": from, "to": to, "step_ms": step.Milliseconds(), "points": points,
	})
}

func historyRange(fromS, toS string, now time.Time) (from, to time.Time, err error) {
	to = now
	if toS != "" {
		if to, err = time.Parse(time.RFC3339Nano, toS); err != nil {
			return from, to, errors.New("to: not an RFC 3339 time")
		}
	}
	from = to.Add(-10 * time.Minute)
	if fromS != "" {
		if from, err = time.Parse(time.RFC3339Nano, fromS); err != nil {
			return from, to, errors.New("from: not an RFC 3339 time")
		}
	}
	switch {
	case !from.Before(to):
		return from, to, errors.New("from must be before to")
	case to.Sub(from) > maxHistory:
		return from, to, fmt.Errorf("a replay can cover %v at most", maxHistory)
	}
	return from, to, nil
}
