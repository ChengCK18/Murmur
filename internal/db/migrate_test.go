package db

import (
	"context"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/ChengCK18/murmur/internal/db/dbtest"
)

func TestMigrate(t *testing.T) {
	pool := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()

	applied, err := Migrate(ctx, pool)
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(applied, []string{"0001_telemetry.sql"}) {
		t.Errorf("applied %v", applied)
	}
	again, err := Migrate(ctx, pool)
	if err != nil || len(again) != 0 {
		t.Errorf("second run applied %v, err %v; want nothing", again, err)
	}

	// The hypertable, its one-day chunks and both policies exist.
	var interval time.Duration
	if err := pool.QueryRow(ctx, `SELECT time_interval FROM timescaledb_information.dimensions
		WHERE hypertable_name = 'telemetry'`).Scan(&interval); err != nil {
		t.Fatal(err)
	}
	if interval != 24*time.Hour {
		t.Errorf("chunk interval %v, want 24h", interval)
	}
	rows, err := pool.Query(ctx, `SELECT proc_name, config->>'drop_after', config->>'compress_after'
		FROM timescaledb_information.jobs WHERE hypertable_name = 'telemetry' ORDER BY proc_name`)
	if err != nil {
		t.Fatal(err)
	}
	var jobs []string
	for rows.Next() {
		var name string
		var drop, compress *string
		if err := rows.Scan(&name, &drop, &compress); err != nil {
			t.Fatal(err)
		}
		switch {
		case drop != nil:
			jobs = append(jobs, name+" drop after "+*drop)
		case compress != nil:
			jobs = append(jobs, name+" compress after "+*compress)
		default:
			jobs = append(jobs, name)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	want := []string{"policy_compression compress after 1 day", "policy_retention drop after 7 days"}
	if !slices.Equal(jobs, want) {
		t.Errorf("jobs %q, want %q", jobs, want)
	}

	var tenant string
	if err := pool.QueryRow(ctx, "SELECT name FROM tenants WHERE id = 1").Scan(&tenant); err != nil || tenant != "default" {
		t.Errorf("default tenant %q, err %v", tenant, err)
	}
}

func TestDedupIndex(t *testing.T) {
	pool := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := Migrate(ctx, pool); err != nil {
		t.Fatal(err)
	}
	insert := `INSERT INTO telemetry (tenant_id, drone_id, msg_type, received_at, sys_id, comp_id, seq, payload)
		VALUES (1, 42, 'HEARTBEAT', '2026-11-01 10:00:00.123456+00', 1, 1, 0, '{}')
		ON CONFLICT DO NOTHING`
	for i, want := range []int64{1, 0} { // the redelivered copy is dropped
		tag, err := pool.Exec(ctx, insert)
		if err != nil {
			t.Fatal(err)
		}
		if tag.RowsAffected() != want {
			t.Errorf("insert %d: %d rows, want %d", i, tag.RowsAffected(), want)
		}
	}
}

func TestConcurrentMigrate(t *testing.T) {
	// Services starting together: the lock makes one apply and the others wait.
	pool := dbtest.New(t)
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()
	var wg sync.WaitGroup
	results := make([][]string, 4)
	errs := make([]error, 4)
	for i := range results {
		wg.Go(func() { results[i], errs[i] = Migrate(ctx, pool) })
	}
	wg.Wait()
	total := 0
	for i := range results {
		if errs[i] != nil {
			t.Fatal(errs[i])
		}
		total += len(results[i])
	}
	if total != 1 {
		t.Errorf("migration applied %d times, want 1: %v", total, results)
	}
}
