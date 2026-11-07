package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ChengCK18/murmur/internal/command"
)

// pgStore keeps the commands table.
type pgStore struct {
	pool   *pgxpool.Pool
	tenant int64
}

func (s pgStore) insert(ctx context.Context, c command.Command, status, reason string) error {
	params, err := json.Marshal(c.Params)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO commands
		(id, tenant_id, drone_id, type, params, issued_by, issued_at, expires_at, status, reason, updated_at, fleet_id)
		VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, NULLIF($10, ''), $7, NULLIF($11, '')::uuid)`,
		c.CommandID, s.tenant, c.DroneID, c.Type, string(params), c.IssuedBy, c.IssuedAt, c.ExpiresAt, status, reason, c.FleetID)
	if err != nil {
		return fmt.Errorf("commands insert: %w", err)
	}
	return nil
}

func (s pgStore) update(ctx context.Context, u command.Update) error {
	_, err := s.pool.Exec(ctx, `UPDATE commands
		SET status = $2, reason = NULLIF($3, ''), attempts = $4, mav_result = $5, updated_at = $6
		WHERE id = $1::uuid`,
		u.CommandID, u.Status, u.Reason, u.Attempts, u.MavResult, u.At)
	if err != nil {
		return fmt.Errorf("commands update: %w", err)
	}
	return nil
}

// markUnknown closes commands a previous run was still tracking and returns
// them. Whether the drone carried them out can no longer be told from here, so
// the UI points at its state, which telemetry still shows (plan §4).
func (s pgStore) markUnknown(ctx context.Context) ([]command.Update, error) {
	rows, err := s.pool.Query(ctx, `UPDATE commands
		SET status = $1, reason = 'the command service restarted', updated_at = now()
		WHERE status IN ($2, $3)
		RETURNING `+updateColumns, command.Unknown, command.Pending, command.Sent)
	if err != nil {
		return nil, fmt.Errorf("commands unknown: %w", err)
	}
	defer rows.Close()
	return pgx.CollectRows(rows, scanUpdate)
}

// recent returns a drone's latest commands, newest first.
func (s pgStore) recent(ctx context.Context, droneID int64, limit int) ([]command.Update, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+updateColumns+` FROM commands
		WHERE drone_id = $1 ORDER BY issued_at DESC LIMIT $2`, droneID, limit)
	if err != nil {
		return nil, fmt.Errorf("commands recent: %w", err)
	}
	return pgx.CollectRows(rows, scanUpdate)
}

func (s pgStore) insertMission(ctx context.Context, c command.Command, r command.MissionRequest) error {
	points, err := json.Marshal(r.Waypoints)
	if err != nil {
		return err
	}
	_, err = s.pool.Exec(ctx, `INSERT INTO missions
		(id, tenant_id, drone_id, waypoints, altitude_m, status, issued_by, created_at, updated_at)
		VALUES ($1::uuid, $2, $3, $4::jsonb, $5, 'uploading', $6, $7, $7)`,
		c.CommandID, s.tenant, c.DroneID, string(points), r.AltitudeM, c.IssuedBy, c.IssuedAt)
	if err != nil {
		return fmt.Errorf("missions insert: %w", err)
	}
	return nil
}

func (s pgStore) finishMission(ctx context.Context, id string, uploaded bool) error {
	status := "failed"
	if uploaded {
		status = "uploaded"
	}
	if _, err := s.pool.Exec(ctx, `UPDATE missions SET status = $2, updated_at = now() WHERE id = $1::uuid`, id, status); err != nil {
		return fmt.Errorf("missions update: %w", err)
	}
	return nil
}

// mission returns a drone's newest mission, or nil.
func (s pgStore) mission(ctx context.Context, droneID int64) (*command.MissionView, error) {
	var v command.MissionView
	var points []byte
	err := s.pool.QueryRow(ctx, `SELECT waypoints, altitude_m, status FROM missions
		WHERE drone_id = $1 ORDER BY created_at DESC LIMIT 1`, droneID).Scan(&points, &v.AltitudeM, &v.Status)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("missions get: %w", err)
	}
	return &v, json.Unmarshal(points, &v.Waypoints)
}

// fleet returns every command of a fleet command.
func (s pgStore) fleet(ctx context.Context, fleetID string) ([]command.Update, error) {
	rows, err := s.pool.Query(ctx, `SELECT `+updateColumns+` FROM commands
		WHERE fleet_id = $1::uuid ORDER BY drone_id`, fleetID)
	if err != nil {
		return nil, fmt.Errorf("commands fleet: %w", err)
	}
	return pgx.CollectRows(rows, scanUpdate)
}

const updateColumns = `id::text, COALESCE(fleet_id::text, ''), drone_id, type, status, COALESCE(reason, ''), attempts, mav_result, issued_by, updated_at`

func scanUpdate(row pgx.CollectableRow) (command.Update, error) {
	var u command.Update
	var result *int16
	err := row.Scan(&u.CommandID, &u.FleetID, &u.DroneID, &u.Type, &u.Status, &u.Reason, &u.Attempts, &result, &u.IssuedBy, &u.At)
	if result != nil {
		r := uint8(*result)
		u.MavResult = &r
	}
	return u, err
}
