package main

import (
	"context"
	"encoding/json"
	"errors"
	"log"
	"sync/atomic"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/nats-io/nats.go/jetstream"
	"github.com/redis/go-redis/v9"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/schema"
)

// consumerConfig is the durable consumer the database writer reads with. A
// restarted consumer resumes after the last acknowledged message.
func consumerConfig(name string, batch int) jetstream.ConsumerConfig {
	return jetstream.ConsumerConfig{
		Durable:       name,
		FilterSubject: bus.TelemetrySubjects,
		AckPolicy:     jetstream.AckExplicitPolicy,
		AckWait:       30 * time.Second, // unacknowledged for this long: redelivered
		MaxAckPending: 20 * batch,
		DeliverPolicy: jetstream.DeliverAllPolicy,
	}
}

type stats struct {
	fetched    atomic.Uint64 // messages read from the stream
	inserted   atomic.Uint64 // rows new to the database
	duplicates atomic.Uint64 // rows already stored: redeliveries
	rejected   atomic.Uint64 // messages that are not valid telemetry, terminated
	failed     atomic.Uint64 // batches not written, left for redelivery
}

type consumer struct {
	cons  jetstream.Consumer
	pool  *pgxpool.Pool
	rdb   *redis.Client
	batch int
	wait  time.Duration
	stats stats
}

// run fetches and writes batches until ctx is cancelled.
func (c *consumer) run(ctx context.Context) {
	for ctx.Err() == nil {
		mb, err := c.cons.Fetch(c.batch, jetstream.FetchMaxWait(c.wait))
		if err != nil {
			if ctx.Err() == nil {
				log.Printf("fetch: %v", err)
				sleep(ctx, time.Second)
			}
			continue
		}
		var msgs []jetstream.Msg
		for m := range mb.Messages() {
			msgs = append(msgs, m)
		}
		if err := mb.Error(); err != nil && !errors.Is(err, jetstream.ErrNoMessages) && ctx.Err() == nil {
			log.Printf("fetch: %v", err)
		}
		if len(msgs) > 0 {
			c.handle(ctx, msgs)
		}
	}
}

// handle writes one batch to the database and Redis, and acknowledges it only
// once both have succeeded. On failure nothing is acknowledged: JetStream
// redelivers the batch, the database drops the rows it already has, and
// Redis gets another try.
func (c *consumer) handle(ctx context.Context, msgs []jetstream.Msg) {
	c.stats.fetched.Add(uint64(len(msgs)))

	var good []jetstream.Msg
	var tels []schema.Telemetry
	var encoded [][]byte
	var rows []row
	for _, m := range msgs {
		var t schema.Telemetry
		err := json.Unmarshal(m.Data(), &t)
		var r row
		if err == nil {
			r, err = newRow(t)
		}
		if err != nil {
			log.Printf("rejecting %s: %v", m.Subject(), err)
			c.stats.rejected.Add(1)
			m.Term() //nolint:errcheck // redelivering it would fail the same way
			continue
		}
		good = append(good, m)
		tels = append(tels, t)
		encoded = append(encoded, m.Data())
		rows = append(rows, r)
	}
	if len(good) == 0 {
		return
	}

	// A batch already fetched is finished even when shutdown starts, so
	// stopping the consumer does not throw away work in hand.
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Second)
	defer cancel()
	err := c.write(wctx, rows, tels, encoded)
	if err != nil {
		c.stats.failed.Add(1)
		if ctx.Err() == nil {
			log.Printf("batch of %d not written, will be redelivered: %v", len(good), err)
		}
		for _, m := range good {
			m.NakWithDelay(time.Second) //nolint:errcheck // AckWait redelivers it anyway
		}
		return
	}
	for _, m := range good {
		m.Ack() //nolint:errcheck // a lost ack means a redelivery, which the database drops
	}
}

func (c *consumer) write(ctx context.Context, rows []row, tels []schema.Telemetry, encoded [][]byte) error {
	inserted, err := writeDB(ctx, c.pool, rows)
	if err != nil {
		return err
	}
	c.stats.inserted.Add(uint64(inserted))
	c.stats.duplicates.Add(uint64(int64(len(rows)) - inserted))
	return writeState(ctx, c.rdb, tels, encoded)
}

func sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}
