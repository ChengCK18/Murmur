// Package db holds Murmur's PostgreSQL (TimescaleDB) access.
package db

import (
	"context"
	"fmt"
	"io/fs"
	"slices"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ChengCK18/murmur/migrations"
)

// migrateLock is the advisory lock key held while migrating, so services
// starting at the same time do not apply the same file twice.
const migrateLock = 0x6d75726d // "murm"

// Migrate applies every migration in migrations.FS that has not been applied
// yet, each in its own transaction, and returns the ones it applied.
func Migrate(ctx context.Context, pool *pgxpool.Pool) ([]string, error) {
	conn, err := pool.Acquire(ctx)
	if err != nil {
		return nil, err
	}
	defer conn.Release()

	if _, err := conn.Exec(ctx, "SELECT pg_advisory_lock($1)", migrateLock); err != nil {
		return nil, fmt.Errorf("db: migration lock: %w", err)
	}
	defer conn.Exec(context.WithoutCancel(ctx), "SELECT pg_advisory_unlock($1)", migrateLock) //nolint:errcheck

	if _, err := conn.Exec(ctx, `CREATE TABLE IF NOT EXISTS schema_migrations (
		version    text        PRIMARY KEY,
		applied_at timestamptz NOT NULL DEFAULT now()
	)`); err != nil {
		return nil, fmt.Errorf("db: schema_migrations: %w", err)
	}

	names, err := fs.Glob(migrations.FS, "*.sql")
	if err != nil {
		return nil, err
	}
	slices.Sort(names)

	var applied []string
	for _, name := range names {
		done, err := apply(ctx, conn.Conn(), name)
		if err != nil {
			return applied, err
		}
		if done {
			applied = append(applied, name)
		}
	}
	return applied, nil
}

// apply runs one migration file unless it has been applied already.
func apply(ctx context.Context, conn *pgx.Conn, name string) (bool, error) {
	sql, err := migrations.FS.ReadFile(name)
	if err != nil {
		return false, err
	}
	tx, err := conn.Begin(ctx)
	if err != nil {
		return false, err
	}
	defer tx.Rollback(ctx) //nolint:errcheck

	var exists bool
	if err := tx.QueryRow(ctx, "SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)", name).Scan(&exists); err != nil {
		return false, err
	}
	if exists {
		return false, nil
	}
	// Without arguments, Exec sends the file as one simple-protocol query, so
	// it may hold many statements.
	if _, err := tx.Exec(ctx, string(sql)); err != nil {
		return false, fmt.Errorf("db: migration %s: %w", name, err)
	}
	if _, err := tx.Exec(ctx, "INSERT INTO schema_migrations (version) VALUES ($1)", name); err != nil {
		return false, err
	}
	return true, tx.Commit(ctx)
}
