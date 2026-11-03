// Command fakegen simulates a fleet of drones sending MAVLink to the
// ingester (guide step 2.8). Each drone has its own UDP socket, and so its
// own source port, and its own frame writer, and so its own sequence counter,
// as real SITL instances do.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"net"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialect"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/frame"
)

func main() {
	count := flag.Int("count", 10, "number of drones")
	target := flag.String("target", "127.0.0.1:14560", "ingester UDP address")
	duration := flag.Duration("duration", 0, "stop after this long; 0 for no limit")
	messages := flag.Int64("messages", 0, "send exactly this many messages in total, then stop; 0 for no limit")
	flag.Parse()
	log.SetFlags(log.Ltime)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	if *duration > 0 {
		var cancel context.CancelFunc
		ctx, cancel = context.WithTimeout(ctx, *duration)
		defer cancel()
	}
	log.Printf("%d drones sending to %s", *count, *target)
	res, err := run(ctx, config{count: *count, target: *target, messages: *messages})
	if err != nil {
		log.Fatalf("fakegen: %v", err)
	}
	log.Printf("sent %d messages, %d send errors", res.sent, res.errors)
}

type config struct {
	count    int
	target   string
	messages int64 // 0: no limit
}

type result struct {
	sent, errors int64
}

// budget hands out the -messages allowance across all drones.
type budget struct {
	limited bool
	left    atomic.Int64
}

func (b *budget) take() bool {
	return !b.limited || b.left.Add(-1) >= 0
}

// run sends until ctx ends or the message budget is used up.
func run(ctx context.Context, cfg config) (result, error) {
	addr, err := net.ResolveUDPAddr("udp", cfg.target)
	if err != nil {
		return result{}, err
	}
	rw, err := dialect.NewReadWriter(ardupilotmega.Dialect)
	if err != nil {
		return result{}, err
	}
	b := &budget{limited: cfg.messages > 0}
	b.left.Store(cfg.messages)

	var sent, errs atomic.Int64
	var wg sync.WaitGroup
	for i := range cfg.count {
		conn, err := net.DialUDP("udp", nil, addr) // a socket, and so a source port, per drone
		if err != nil {
			return result{}, fmt.Errorf("drone %d: %w", i, err)
		}
		w, err := frame.NewWriter(frame.WriterConf{
			Writer:         conn, // one Write per frame: one datagram per frame, as SITL sends
			DialectRW:      rw,
			OutVersion:     frame.V2,
			OutSystemID:    sysID(i),
			OutComponentID: 1,
		})
		if err != nil {
			conn.Close()
			return result{}, err
		}
		wg.Go(func() {
			defer conn.Close()
			// Spread the drones over one tick, so they do not all send in the
			// same millisecond.
			offset := time.Duration(i) * tick / time.Duration(cfg.count)
			fly(ctx, newPath(i, cfg.count), w, offset, b, &sent, &errs)
		})
	}
	wg.Wait()
	return result{sent: sent.Load(), errors: errs.Load()}, nil
}

// sysID gives drone i a MAVLink system ID from 1 to 250 (255 is a ground
// station). IDs repeat beyond 250 drones; the ingester tells drones apart by
// source address as well.
func sysID(i int) uint8 {
	return uint8(1 + i%250)
}

func fly(ctx context.Context, p path, w *frame.Writer, offset time.Duration, b *budget, sent, errs *atomic.Int64) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(offset):
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for n := 0; ; n++ {
		for _, m := range p.messages(n) {
			if !b.take() {
				return
			}
			if err := w.WriteMessage(m); err != nil {
				errs.Add(1)
				continue
			}
			sent.Add(1)
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
