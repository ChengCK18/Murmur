package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sync/atomic"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ChengCK18/murmur/internal/alert"
)

type store struct {
	pool   *pgxpool.Pool
	tenant int64
}

// event records an alert in the events table (plan §7).
func (s store) event(ctx context.Context, a alert.Alert) error {
	details, err := json.Marshal(a)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO events (tenant_id, drone_id, at, type, details)
		VALUES ($1, $2, $3, 'alert', $4::jsonb)`, s.tenant, a.DroneID, a.At, string(details))
	if err != nil {
		return fmt.Errorf("events insert: %w", err)
	}
	return nil
}

func (s store) fence(ctx context.Context) (alert.Fence, error) {
	var f alert.Fence
	var poly []byte
	err := s.pool.QueryRow(ctx, `SELECT polygon, updated_by, updated_at FROM geofences WHERE tenant_id = $1`,
		s.tenant).Scan(&poly, &f.UpdatedBy, &f.UpdatedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return alert.Fence{Polygon: []alert.Point{}}, nil
	}
	if err != nil {
		return f, fmt.Errorf("geofence: %w", err)
	}
	return f, json.Unmarshal(poly, &f.Polygon)
}

func (s store) setFence(ctx context.Context, f alert.Fence) error {
	if f.Polygon == nil {
		f.Polygon = []alert.Point{}
	}
	poly, err := json.Marshal(f.Polygon)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO geofences (tenant_id, polygon, updated_by, updated_at)
		VALUES ($1, $2::jsonb, $3, $4)
		ON CONFLICT (tenant_id) DO UPDATE SET polygon = $2::jsonb, updated_by = $3, updated_at = $4`,
		s.tenant, string(poly), f.UpdatedBy, f.UpdatedAt)
	if err != nil {
		return fmt.Errorf("geofence save: %w", err)
	}
	return nil
}

type metricsCount struct{ n atomic.Uint64 }

func (c *metricsCount) inc()         { c.n.Add(1) }
func (c *metricsCount) get() float64 { return float64(c.n.Load()) }
