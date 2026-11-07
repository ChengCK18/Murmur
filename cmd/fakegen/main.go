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
	"math/rand/v2"
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

	"github.com/ChengCK18/murmur/internal/mavlink"
)

func main() {
	count := flag.Int("count", 10, "number of drones")
	target := flag.String("target", "127.0.0.1:14560", "ingester UDP address")
	duration := flag.Duration("duration", 0, "stop after this long; 0 for no limit")
	messages := flag.Int64("messages", 0, "send exactly this many messages in total, then stop; 0 for no limit")
	drop := flag.Float64("drop", 0, "share of messages to lose on purpose, from 0 to 1, after they take a sequence number")
	commandDrop := flag.Float64("command-drop", 0, "share of commands a drone loses without answering, from 0 to 1")
	commandReject := flag.Float64("command-reject", 0, "share of commands a drone rejects, from 0 to 1")
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
	res, err := run(ctx, config{count: *count, target: *target, messages: *messages, drop: *drop,
		commandDrop: *commandDrop, commandReject: *commandReject})
	if err != nil {
		log.Fatalf("fakegen: %v", err)
	}
	// loadtest/failures reads this line.
	log.Printf("sent %d messages, %d send errors, %d dropped", res.sent, res.errors, res.dropped)
}

type config struct {
	count    int
	target   string
	messages int64 // 0: no limit
	drop     float64
	lookup   func() (*net.UDPAddr, error) // nil: resolve target
	refresh  time.Duration                // between lookups; 0: lookupEvery

	commandDrop, commandReject float64
}

// lookupEvery is how often the target is looked up again.
const lookupEvery = 5 * time.Second

type result struct {
	sent, errors int64 // sent includes the dropped messages
	dropped      int64
}

// destination is the ingester's current address. A restarted ingester can come
// back on a new IP address, and a real drone's ground link would follow it, so
// the name is looked up again while the fleet flies. Each drone keeps its
// socket, so the ingester still sees the same source port.
type destination struct {
	addr   atomic.Pointer[net.UDPAddr]
	lookup func() (*net.UDPAddr, error)
}

func (d *destination) follow(ctx context.Context, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
		addr, err := d.lookup()
		if err != nil {
			continue // the name goes while the ingester is down: keep the last address
		}
		if old := d.addr.Swap(addr); old.String() != addr.String() {
			log.Printf("target moved from %v to %v", old, addr)
		}
	}
}

// link is one drone's radio: each Write is one datagram, as SITL sends. A
// dropped frame still took a sequence number, so the receiver sees a gap, the
// same as a packet lost on the way.
type link struct {
	conn    *net.UDPConn
	to      *destination
	drop    float64
	dropped *atomic.Int64
}

func (l *link) Write(p []byte) (int, error) {
	if l.drop > 0 && rand.Float64() < l.drop {
		l.dropped.Add(1)
		return len(p), nil
	}
	return l.conn.WriteToUDP(p, l.to.addr.Load())
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
	lookup := cfg.lookup
	if lookup == nil {
		lookup = func() (*net.UDPAddr, error) { return net.ResolveUDPAddr("udp", cfg.target) }
	}
	addr, err := lookup()
	if err != nil {
		return result{}, err
	}
	to := &destination{lookup: lookup}
	to.addr.Store(addr)
	refresh := cfg.refresh
	if refresh == 0 {
		refresh = lookupEvery
	}
	followCtx, stopFollowing := context.WithCancel(ctx)
	defer stopFollowing()
	go to.follow(followCtx, refresh)

	rw, err := dialect.NewReadWriter(ardupilotmega.Dialect)
	if err != nil {
		return result{}, err
	}
	b := &budget{limited: cfg.messages > 0}
	b.left.Store(cfg.messages)

	dec, err := mavlink.NewDecoder()
	if err != nil {
		return result{}, err
	}
	var sent, errs, dropped atomic.Int64
	var wg sync.WaitGroup
	for i := range cfg.count {
		conn, err := net.ListenUDP("udp", nil) // a socket, and so a source port, per drone
		if err != nil {
			return result{}, fmt.Errorf("drone %d: %w", i, err)
		}
		w, err := frame.NewWriter(frame.WriterConf{
			Writer:         &link{conn: conn, to: to, drop: cfg.drop, dropped: &dropped},
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
			defer conn.Close() // ends respond
			r := &radio{w: w}
			pl := newPilot(newPath(i, cfg.count))
			go respond(conn, dec, r, pl, sysID(i), cfg.commandDrop, cfg.commandReject)
			// Spread the drones over one tick, so they do not all send in the
			// same millisecond.
			offset := time.Duration(i) * tick / time.Duration(cfg.count)
			fly(ctx, pl, r, offset, b, &sent, &errs)
		})
	}
	wg.Wait()
	return result{sent: sent.Load(), errors: errs.Load(), dropped: dropped.Load()}, nil
}

// sysID gives drone i a MAVLink system ID from 1 to 250 (255 is a ground
// station). IDs repeat beyond 250 drones; the ingester tells drones apart by
// source address as well.
func sysID(i int) uint8 {
	return uint8(1 + i%250)
}

func fly(ctx context.Context, pl *pilot, r *radio, offset time.Duration, b *budget, sent, errs *atomic.Int64) {
	select {
	case <-ctx.Done():
		return
	case <-time.After(offset):
	}
	t := time.NewTicker(tick)
	defer t.Stop()
	for n := 0; ; n++ {
		for _, m := range frames(n, pl.at(time.Duration(n)*tick)) {
			if !b.take() {
				return
			}
			if err := r.send(m); err != nil {
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
