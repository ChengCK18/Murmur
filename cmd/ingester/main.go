// Command ingester receives MAVLink over UDP, works out which drone sent each
// frame, tracks packet loss and link state, and prints the state of every
// drone once a second.
//
// Phase 1 of the guide: console output only. Publishing to NATS comes in
// Phase 2.
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

	"github.com/ChengCK18/murmur/internal/linkstats"
)

func main() {
	listen := flag.String("listen", ":14560", "UDP address to receive MAVLink on")
	interval := flag.Duration("interval", time.Second, "how often to print drone state")
	timeout := flag.Duration("timeout", linkstats.DefaultTimeout, "time without a heartbeat before a link counts as lost")
	flag.Parse()
	log.SetFlags(0)

	if err := run(*listen, *interval, *timeout); err != nil {
		log.Fatalf("ingester: %v", err)
	}
}

func run(listen string, interval, timeout time.Duration) error {
	addr, err := net.ResolveUDPAddr("udp", listen)
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

	in, err := newIngester(timeout, os.Stdout)
	if err != nil {
		return err
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-ctx.Done()
		conn.Close() // unblocks ReadFromUDPAddrPort
	}()
	go func() {
		t := time.NewTicker(interval)
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
