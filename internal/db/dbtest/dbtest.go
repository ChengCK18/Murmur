// Package dbtest gives tests a fresh, empty TimescaleDB database.
//
// It connects to MURMUR_TEST_DATABASE_URL, or to the Compose database
// (deploy/docker-compose.yml) if that is unset, creates a database for the
// test and drops it afterwards. If no server is reachable the test is
// skipped, so go test ./... works without Docker.
package dbtest

import (
	"context"
	"fmt"
	"os"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const defaultURL = "postgres://murmur:murmur@127.0.0.1:5432/murmur"

var counter atomic.Int64

// New returns a pool connected to a new, empty database.
func New(t testing.TB) *pgxpool.Pool {
	t.Helper()
	adminURL := os.Getenv("MURMUR_TEST_DATABASE_URL")
	if adminURL == "" {
		adminURL = defaultURL
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	admin, err := pgx.Connect(ctx, adminURL)
	if err != nil {
		t.Skipf("dbtest: no database at %s (start deploy/docker-compose.yml or set MURMUR_TEST_DATABASE_URL): %v", adminURL, err)
	}
	defer admin.Close(context.Background())

	name := fmt.Sprintf("murmur_test_%d_%d", os.Getpid(), counter.Add(1))
	if _, err := admin.Exec(ctx, "CREATE DATABASE "+name); err != nil {
		t.Fatal(err)
	}

	cfg, err := pgxpool.ParseConfig(adminURL)
	if err != nil {
		t.Fatal(err)
	}
	cfg.ConnConfig.Database = name
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		pool.Close()
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		admin, err := pgx.Connect(ctx, adminURL)
		if err != nil {
			t.Errorf("dbtest: drop %s: %v", name, err)
			return
		}
		defer admin.Close(ctx)
		if _, err := admin.Exec(ctx, "DROP DATABASE "+name+" WITH (FORCE)"); err != nil {
			t.Errorf("dbtest: drop %s: %v", name, err)
		}
	})
	return pool
}
