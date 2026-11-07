package main

import (
	"context"
	"encoding/json"
	"fmt"

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

// markUnknown closes commands a previous run was still tracking. Whether the
// drone carried them out can no longer be told from here, so the UI points at
// its state, which telemetry still shows (plan §4).
func (s pgStore) markUnknown(ctx context.Context) (int64, error) {
	tag, err := s.pool.Exec(ctx, `UPDATE commands
		SET status = $1, reason = 'the command service restarted', updated_at = now()
		WHERE status IN ($2, $3)`, command.Unknown, command.Pending, command.Sent)
	if err != nil {
		return 0, fmt.Errorf("commands unknown: %w", err)
	}
	return tag.RowsAffected(), nil
}
