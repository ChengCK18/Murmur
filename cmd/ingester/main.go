// Command ingester receives MAVLink over UDP, works out which drone sent each
// frame, tracks packet loss and link state, and prints the state of every
// drone once a second.
//
// Telemetry from drones is published to the TELEMETRY JetStream stream as
// schema.Telemetry JSON (guide step 2.4); -nats "" turns that off.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log"
	"net"
	"net/netip"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/db"
	"github.com/ChengCK18/murmur/internal/linkstats"
	"github.com/ChengCK18/murmur/internal/registry"
)

func main() {
	listen := flag.String("listen", ":14560", "UDP address to receive MAVLink on")
	interval := flag.Duration("interval", time.Second, "how often to print drone state")
	timeout := flag.Duration("timeout", linkstats.DefaultTimeout, "time without a heartbeat before a link counts as lost")
	natsURL := flag.String("nats", "nats://127.0.0.1:4222", `NATS server to publish telemetry to; "" for console only`)
	maxPending := flag.Int("max-pending", 4096, "most JetStream publishes awaiting acknowledgement")
	tenant := flag.Int64("tenant", 1, "tenant ID stamped on every message")
	dbURL := flag.String("db", "postgres://murmur:murmur@127.0.0.1:5432/murmur", `TimescaleDB holding drone IDs; "" to keep them in memory`)
	flag.Parse()
	log.SetFlags(0)

	cfg := config{listen: *listen, interval: *interval, timeout: *timeout, natsURL: *natsURL, maxPending: *maxPending, tenant: *tenant, dbURL: *dbURL}
	if err := run(cfg); err != nil {
		log.Fatalf("ingester: %v", err)
	}
}

type config struct {
	listen            string
	interval, timeout time.Duration
	natsURL           string
	maxPending        int
	tenant            int64
	dbURL             string
}

func run(cfg config) error {
	addr, err := net.ResolveUDPAddr("udp", cfg.listen)
	if err != nil {
		return err
	}
	// The ingester owns the socket (rather than letting gomavlib open it) so
	// that Phase 5 can send commands back from the address drones send to.
	conn, err := net.ListenUDP("udp", addr)
	if err != nil {
		return err
	}
	defer conn.Close()

	in, err := newIngester(cfg.timeout, os.Stdout)
	if err != nil {
		return err
	}
	in.tenant = cfg.tenant

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if cfg.dbURL != "" {
		setup, cancel := context.WithTimeout(ctx, 30*time.Second)
		reg, closeDB, err := openRegistry(setup, cfg.dbURL, cfg.tenant, cfg.timeout)
		cancel()
		if err != nil {
			return err
		}
		defer closeDB()
		in.reg = reg
		fmt.Printf("drone IDs stored in %s\n", redact(cfg.dbURL))
	}

	if cfg.natsURL != "" {
		setup, cancel := context.WithTimeout(ctx, 10*time.Second)
		p, err := newNATSPublisher(setup, cfg.natsURL, cfg.maxPending)
		cancel()
		if err != nil {
			return err
		}
		defer p.close(5 * time.Second)
		in.pub = p
		fmt.Printf("publishing to %s, stream %s\n", cfg.natsURL, bus.TelemetryStream)
	}
	go func() {
		<-ctx.Done()
		conn.Close() // unblocks ReadFromUDPAddrPort
	}()
	go func() {
		t := time.NewTicker(cfg.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case now := <-t.C:
				fmt.Print(in.tick(now))
			}
		}
	}()

	fmt.Printf("listening on %v (Ctrl-C to stop)\n", conn.LocalAddr())
	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if ctx.Err() != nil || errors.Is(err, net.ErrClosed) {
				break
			}
			log.Printf("read: %v", err)
			continue
		}
		// On a dual-stack socket IPv4 senders arrive as ::ffff:a.b.c.d;
		// unmap them so addresses print and compare as plain IPv4.
		src = netip.AddrPortFrom(src.Addr().Unmap(), src.Port())
		in.handlePacket(time.Now(), src, buf[:n])
	}

	fmt.Print("\nstopped\n", in.tick(time.Now()))
	return nil
}

// openRegistry returns a registry backed by the drones table, so drone IDs
// survive an ingester restart. It applies migrations first: the ingester may
// start before the consumer has created the table.
func openRegistry(ctx context.Context, url string, tenant int64, timeout time.Duration) (*registry.Registry, func(), error) {
	pool, err := pgxpool.New(ctx, url)
	if err != nil {
		return nil, nil, err
	}
	if _, err := db.Migrate(ctx, pool); err != nil {
		pool.Close()
		return nil, nil, err
	}
	reg, err := registry.NewWithStore(ctx, timeout, db.Drones{Pool: pool, TenantID: tenant})
	if err != nil {
		pool.Close()
		return nil, nil, err
	}
	return reg, pool.Close, nil
}

// redact hides the password in a database URL for printing.
func redact(raw string) string {
	u, err := url.Parse(raw)
	if err != nil {
		return "the database"
	}
	return u.Redacted()
}
