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
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/linkstats"
)

func main() {
	listen := flag.String("listen", ":14560", "UDP address to receive MAVLink on")
	interval := flag.Duration("interval", time.Second, "how often to print drone state")
	timeout := flag.Duration("timeout", linkstats.DefaultTimeout, "time without a heartbeat before a link counts as lost")
	natsURL := flag.String("nats", "nats://127.0.0.1:4222", `NATS server to publish telemetry to; "" for console only`)
	maxPending := flag.Int("max-pending", 4096, "most JetStream publishes awaiting acknowledgement")
	tenant := flag.Int64("tenant", 1, "tenant ID stamped on every message")
	flag.Parse()
	log.SetFlags(0)

	cfg := config{listen: *listen, interval: *interval, timeout: *timeout, natsURL: *natsURL, maxPending: *maxPending, tenant: *tenant}
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
