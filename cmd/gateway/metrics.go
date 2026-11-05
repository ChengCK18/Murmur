package main

import (
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChengCK18/murmur/internal/metrics"
)

func (s *server) registerMetrics(reg prometheus.Registerer) {
	h := s.hub
	metrics.Gauge(reg, "murmur_gateway_clients", "Browsers connected.", func() float64 { return float64(s.clients.Load()) })
	metrics.Gauge(reg, "murmur_gateway_drones", "Drones with state in the gateway.", func() float64 {
		h.mu.Lock()
		defer h.mu.Unlock()
		return float64(len(h.drones))
	})
	metrics.Counter(reg, "murmur_gateway_feed_messages_total", "Telemetry messages read from JetStream.", func() float64 { return float64(h.feedMessages.Load()) })
	metrics.Counter(reg, "murmur_gateway_feed_errors_total", "Telemetry messages that could not be applied.", func() float64 { return float64(h.feedErrors.Load()) })
	metrics.Counter(reg, "murmur_gateway_messages_sent_total", "Messages written to browsers.", func() float64 { return float64(s.counts.sent.Load()) })
	metrics.Counter(reg, "murmur_gateway_updates_coalesced_total", "Updates replaced by a newer one before being sent (throttling).", func() float64 { return float64(s.counts.coalesced.Load()) })

	latency := prometheus.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "murmur_gateway_receive_to_send_seconds",
		Help:    "Time from the ingester receiving a frame to the gateway writing it to a browser.",
		Buckets: metrics.LatencyBuckets,
	}, []string{"subscription"})
	byID, byArea := latency.WithLabelValues("drone"), latency.WithLabelValues("area")
	reg.MustRegister(latency)
	s.counts.latency = func(area bool, d time.Duration) {
		if area {
			byArea.Observe(d.Seconds())
		} else {
			byID.Observe(d.Seconds())
		}
	}
}
