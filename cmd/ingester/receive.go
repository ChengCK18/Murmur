package main

import (
	"errors"
	"log"
	"net"
	"net/netip"
	"sync/atomic"
	"time"
)

// packet is one UDP datagram as it left the socket.
type packet struct {
	at   time.Time // received_at: read here, so time in the queue counts as latency
	src  netip.AddrPort
	data []byte
}

// receive reads datagrams into queue until the socket closes, then closes
// queue. Reading is kept apart from processing because UDP has no flow
// control: anything that holds up the read, even for the 30 ms a default
// socket buffer lasts at 10,000 messages a second, loses packets in the
// kernel where nothing counts them. A full queue drops the packet, as the
// kernel would, but dropped counts it.
func receive(conn *net.UDPConn, queue chan<- packet, dropped *atomic.Uint64) {
	defer close(queue)
	buf := make([]byte, 65535)
	for {
		n, src, err := conn.ReadFromUDPAddrPort(buf)
		if err != nil {
			if errors.Is(err, net.ErrClosed) {
				return
			}
			log.Printf("read: %v", err)
			continue
		}
		// On a dual-stack socket IPv4 senders arrive as ::ffff:a.b.c.d;
		// unmap them so addresses print and compare as plain IPv4.
		p := packet{at: time.Now(), src: netip.AddrPortFrom(src.Addr().Unmap(), src.Port()), data: append([]byte(nil), buf[:n]...)}
		select {
		case queue <- p:
		default:
			dropped.Add(1)
		}
	}
}
