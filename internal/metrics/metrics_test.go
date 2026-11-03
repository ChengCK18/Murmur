package metrics

import (
	"context"
	"io"
	"net"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"
)

func TestServe(t *testing.T) {
	reg := NewRegistry()
	n := 0.0
	Counter(reg, "murmur_test_things_total", "Things.", func() float64 { n++; return n })
	Gauge(reg, "murmur_test_level", "Level.", func() float64 { return 7 })
	h := prometheus.NewHistogram(prometheus.HistogramOpts{Name: "murmur_test_seconds", Help: "Time.", Buckets: LatencyBuckets})
	reg.MustRegister(h)
	h.Observe(0.25)
	h.Observe(0.75)

	ln, err := net.Listen("tcp", "127.0.0.1:0") // find a free port
	if err != nil {
		t.Fatal(err)
	}
	addr := ln.Addr().String()
	ln.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	if err := Serve(ctx, addr, reg); err != nil {
		t.Fatal(err)
	}
	resp, err := http.Get("http://" + addr + "/metrics")
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	for _, want := range []string{"murmur_test_things_total 1", "murmur_test_level 7", "murmur_test_seconds_count 2", "go_goroutines"} {
		if !strings.Contains(string(body), want) {
			t.Errorf("no %q in /metrics", want)
		}
	}

	snap, err := Snapshot(reg)
	if err != nil {
		t.Fatal(err)
	}
	if snap["murmur_test_things_total"] != 2 || snap["murmur_test_level"] != 7 ||
		snap["murmur_test_seconds_count"] != 2 || snap["murmur_test_seconds_sum"] != 1 {
		t.Errorf("snapshot %v", snap)
	}

	if err := Serve(ctx, addr, reg); err == nil {
		t.Error("serving twice on one port did not fail")
	}
	cancel()
	time.Sleep(100 * time.Millisecond)
	if _, err := http.Get("http://" + addr + "/metrics"); err == nil {
		t.Error("still serving after the context ended")
	}
}
