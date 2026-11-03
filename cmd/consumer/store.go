package main

import (
	"context"
	"fmt"
	"strconv"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/schema"
	"github.com/ChengCK18/murmur/internal/state"
)

// row is one telemetry row: the message plus the typed columns its type fills.
type row struct {
	t          schema.Telemetry
	lat, lon   *float64
	alt, rel   *float32
	speed, hdg *float32
	battery    *int16
}

// newRow fills the typed columns from the message's payload.
func newRow(t schema.Telemetry) (row, error) {
	r := row{t: t}
	msg, err := t.Message()
	if err != nil {
		return r, err
	}
	switch m := msg.(type) {
	case *ardupilotmega.MessageGlobalPositionInt:
		p := state.PositionOf(m)
		alt, rel, speed := float32(p.AltMSL), float32(p.RelAlt), float32(p.Speed)
		r.lat, r.lon, r.alt, r.rel, r.speed = &p.Lat, &p.Lon, &alt, &rel, &speed
		if p.HeadingDeg != nil {
			hdg := float32(*p.HeadingDeg)
			r.hdg = &hdg
		}
	case *ardupilotmega.MessageSysStatus:
		r.battery = batteryPct(m.BatteryRemaining)
	case *ardupilotmega.MessageBatteryStatus:
		r.battery = batteryPct(m.BatteryRemaining)
	}
	return r, nil
}

func batteryPct(v int8) *int16 {
	p := state.BatteryPercent(v)
	if p == nil {
		return nil
	}
	b := int16(*p)
	return &b
}

const insertTelemetry = `INSERT INTO telemetry (
	tenant_id, drone_id, msg_type, received_at, time_boot_ms, sys_id, comp_id, seq,
	lat, lon, alt_m, rel_alt_m, speed_ms, heading_deg, battery_pct, payload
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
ON CONFLICT DO NOTHING`

// writeDB inserts rows in one transaction and returns how many were new;
// the rest were already stored (redelivered after a restart).
func writeDB(ctx context.Context, pool *pgxpool.Pool, rows []row) (int64, error) {
	var inserted int64
	err := pgx.BeginFunc(ctx, pool, func(tx pgx.Tx) error {
		b := &pgx.Batch{}
		for _, r := range rows {
			t := r.t
			b.Queue(insertTelemetry, t.TenantID, t.DroneID, t.Msg, t.ReceivedAt, t.TimeBootMs,
				int16(t.SysID), int16(t.CompID), int16(t.Seq),
				r.lat, r.lon, r.alt, r.rel, r.speed, r.hdg, r.battery, []byte(t.Payload))
		}
		br := tx.SendBatch(ctx, b)
		for range rows {
			tag, err := br.Exec()
			if err != nil {
				br.Close()
				return err
			}
			inserted += tag.RowsAffected()
		}
		return br.Close()
	})
	if err != nil {
		return 0, fmt.Errorf("telemetry insert: %w", err)
	}
	return inserted, nil
}

// latestState sets one message type in a drone's Redis hash, unless the hash
// already holds a newer message of that type: a redelivered old message must
// not overwrite newer state. Times are microseconds since 1970, which Lua's
// doubles hold exactly (nanoseconds would not).
//
// Hash drone:<tenant_id>:<drone_id>:
//
//	<MSG>       latest message of that type, as schema.Telemetry JSON
//	<MSG>:us    its received_at, in microseconds
//	last_seen   received_at of the newest message of any type (RFC 3339)
//	last_seen:us
var latestState = redis.NewScript(`
local cur = redis.call('HGET', KEYS[1], ARGV[1] .. ':us')
if cur and tonumber(cur) >= tonumber(ARGV[2]) then return 0 end
redis.call('HSET', KEYS[1], ARGV[1], ARGV[3], ARGV[1] .. ':us', ARGV[2])
local seen = redis.call('HGET', KEYS[1], 'last_seen:us')
if not seen or tonumber(seen) < tonumber(ARGV[2]) then
  redis.call('HSET', KEYS[1], 'last_seen', ARGV[4], 'last_seen:us', ARGV[2])
end
return 1
`)

func stateKey(tenantID, droneID int64) string {
	return "drone:" + strconv.FormatInt(tenantID, 10) + ":" + strconv.FormatInt(droneID, 10)
}

// writeState updates Redis in one round trip. Only the newest message of each
// type per drone in the batch is sent; the script drops older ones anyway.
func writeState(ctx context.Context, rdb *redis.Client, msgs []schema.Telemetry, encoded [][]byte) error {
	type field struct {
		key, msg string
	}
	newest := map[field]int{}
	for i, t := range msgs {
		f := field{stateKey(t.TenantID, t.DroneID), t.Msg}
		if j, ok := newest[f]; !ok || t.ReceivedAt.After(msgs[j].ReceivedAt) {
			newest[f] = i
		}
	}
	pipe := rdb.Pipeline()
	for f, i := range newest {
		t := msgs[i]
		latestState.Eval(ctx, pipe, []string{f.key},
			f.msg, t.ReceivedAt.UnixMicro(), encoded[i], t.ReceivedAt.Format("2006-01-02T15:04:05.999999999Z07:00"))
	}
	if _, err := pipe.Exec(ctx); err != nil {
		return fmt.Errorf("redis state: %w", err)
	}
	return nil
}
