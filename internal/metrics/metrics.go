// Package metrics serves Prometheus metrics for Murmur's services (guide
// step 2.7).
package metrics

import (
	"context"
	"errors"
	"log"
	"net"
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

// LatencyBuckets are for end-to-end latencies, from 1 ms to about 16 s.
var LatencyBuckets = prometheus.ExponentialBuckets(0.001, 2, 15)

// NewRegistry returns a registry with the Go runtime and process metrics.
func NewRegistry() *prometheus.Registry {
	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	return reg
}

// Counter registers a counter whose value is read from f at each scrape, for
// totals a service already keeps.
func Counter(reg prometheus.Registerer, name, help string, f func() float64) {
	reg.MustRegister(prometheus.NewCounterFunc(prometheus.CounterOpts{Name: name, Help: help}, f))
}

// Gauge registers a gauge whose value is read from f at each scrape.
func Gauge(reg prometheus.Registerer, name, help string, f func() float64) {
	reg.MustRegister(prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help}, f))
}

// Serve serves /metrics on addr until ctx is cancelled. It returns once the
// port is open, so a bad address fails at start.
func Serve(ctx context.Context, addr string, reg *prometheus.Registry) error {
	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	mux := http.NewServeMux()
	mux.Handle("/metrics", promhttp.HandlerFor(reg, promhttp.HandlerOpts{Registry: reg}))
	srv := &http.Server{Handler: mux, ReadHeaderTimeout: 5 * time.Second}
	go func() {
		<-ctx.Done()
		shutdown, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		srv.Shutdown(shutdown) //nolint:errcheck
	}()
	go func() {
		if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Printf("metrics: %v", err)
		}
	}()
	return nil
}

// Snapshot reads every counter and gauge in g by name, and each histogram as
// <name>_count and <name>_sum. Metrics with labels are left out.
func Snapshot(g prometheus.Gatherer) (map[string]float64, error) {
	families, err := g.Gather()
	if err != nil {
		return nil, err
	}
	out := map[string]float64{}
	for _, f := range families {
		for _, m := range f.GetMetric() {
			if len(m.GetLabel()) > 0 {
				continue
			}
			switch {
			case m.Counter != nil:
				out[f.GetName()] = m.GetCounter().GetValue()
			case m.Gauge != nil:
				out[f.GetName()] = m.GetGauge().GetValue()
			case m.Histogram != nil:
				out[f.GetName()+"_count"] = float64(m.GetHistogram().GetSampleCount())
				out[f.GetName()+"_sum"] = m.GetHistogram().GetSampleSum()
			}
		}
	}
	return out, nil
}
