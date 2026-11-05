package main

import (
	"net"
	"sync/atomic"
	"testing"
	"time"
)

func listen(t *testing.T) (*net.UDPConn, *net.UDPConn) {
	t.Helper()
	ln, err := net.ListenUDP("udp", &net.UDPAddr{IP: net.IPv4(127, 0, 0, 1)})
	if err != nil {
		t.Fatal(err)
	}
	out, err := net.DialUDP("udp", nil, ln.LocalAddr().(*net.UDPAddr))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { ln.Close(); out.Close() })
	return ln, out
}

func TestReceive(t *testing.T) {
	// A stalled processor costs no packets while the queue has room, each
	// keeps the time it was read and its own bytes, and closing the socket
	// ends the queue.
	ln, out := listen(t)
	queue := make(chan packet, 100)
	var dropped atomic.Uint64
	go receive(ln, queue, &dropped)

	start := time.Now()
	for i := range 50 {
		if _, err := out.Write([]byte{byte(i)}); err != nil {
			t.Fatal(err)
		}
	}
	time.Sleep(50 * time.Millisecond) // nothing is reading the queue
	stalled := time.Now()
	ln.Close()
	got := 0
	for p := range queue {
		if p.data[0] != byte(got) {
			t.Fatalf("packet %d holds %d", got, p.data[0])
		}
		if p.at.Before(start) || p.at.After(stalled) {
			t.Errorf("packet %d read at %v, not as it arrived", got, p.at)
		}
		if p.src.Addr().Is4In6() {
			t.Errorf("source %v still mapped", p.src)
		}
		got++
	}
	if got != 50 || dropped.Load() != 0 {
		t.Errorf("%d packets, %d dropped", got, dropped.Load())
	}
}

func TestReceiveCountsWhatItDrops(t *testing.T) {
	ln, out := listen(t)
	queue := make(chan packet, 10)
	var dropped atomic.Uint64
	go receive(ln, queue, &dropped)
	for range 30 {
		out.Write([]byte{1}) //nolint:errcheck
	}
	time.Sleep(50 * time.Millisecond)
	ln.Close()
	got := uint64(0)
	for range queue {
		got++
	}
	if got != 10 || dropped.Load() != 20 {
		t.Errorf("%d queued, %d dropped, want 10 and 20", got, dropped.Load())
	}
}
