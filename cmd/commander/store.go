package main

import (
	"context"
	"encoding/json"
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
		(id, tenant_id, drone_id, type, params, issued_by, issued_at, expires_at, status, reason, updated_at)
		VALUES ($1::uuid, $2, $3, $4, $5::jsonb, $6, $7, $8, $9, NULLIF($10, ''), $7)`,
		c.CommandID, s.tenant, c.DroneID, c.Type, string(params), c.IssuedBy, c.IssuedAt, c.ExpiresAt, status, reason)
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

const updateColumns = `id::text, drone_id, type, status, COALESCE(reason, ''), attempts, mav_result, issued_by, updated_at`

func scanUpdate(row pgx.CollectableRow) (command.Update, error) {
	var u command.Update
	var result *int16
	err := row.Scan(&u.CommandID, &u.DroneID, &u.Type, &u.Status, &u.Reason, &u.Attempts, &result, &u.IssuedBy, &u.At)
	if result != nil {
		r := uint8(*result)
		u.MavResult = &r
	}
	return u, err
}
