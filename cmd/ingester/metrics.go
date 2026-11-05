package main

import (
	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChengCK18/murmur/internal/metrics"
)

// registerMetrics exposes the ingester's counters (guide step 2.7).
func (in *ingester) registerMetrics(reg prometheus.Registerer) {
	locked := func(f func() uint64) func() float64 {
		return func() float64 {
			in.mu.Lock()
			defer in.mu.Unlock()
			return float64(f())
		}
	}
	metrics.Counter(reg, "murmur_ingester_packets_total", "UDP datagrams received.", locked(func() uint64 { return in.packets }))
	metrics.Counter(reg, "murmur_ingester_queue_dropped_total", "Packets dropped because processing fell behind.", func() float64 { return float64(in.queueDropped.Load()) })
	metrics.Counter(reg, "murmur_ingester_frames_total", "MAVLink frames decoded.", locked(func() uint64 { return in.frames }))
	metrics.Counter(reg, "murmur_ingester_decode_errors_total", "Datagrams with bytes that are not valid MAVLink.", locked(func() uint64 { return in.decodeErrors }))
	metrics.Counter(reg, "murmur_ingester_frames_ignored_total", "Frames from ground stations.", locked(func() uint64 { return in.ignored }))
	metrics.Counter(reg, "murmur_ingester_frames_unregistered_total", "Frames from senders not yet registered by a heartbeat.", locked(func() uint64 { return in.unregistered }))
	metrics.Gauge(reg, "murmur_ingester_drones", "Drones seen by this ingester.", locked(func() uint64 { return uint64(len(in.drones)) }))

	metrics.Counter(reg, "murmur_ingester_frames_lost_total", "Frames missing from sequence gaps.", func() float64 { return float64(in.links.Totals().Lost) })
	metrics.Counter(reg, "murmur_ingester_sequence_resets_total", "Sequence restarts (sender restart or reordering), not counted as loss.", func() float64 { return float64(in.links.Totals().Resets) })
	metrics.Counter(reg, "murmur_ingester_link_losses_total", "Times a sender went without a heartbeat for the timeout.", func() float64 { return float64(in.links.Totals().LinkLosses) })
	metrics.Gauge(reg, "murmur_ingester_links_up", "Senders with a heartbeat within the timeout.", func() float64 { return float64(in.links.Totals().LinkUp) })
	metrics.Counter(reg, "murmur_ingester_registry_store_errors_total", "Failed drone ID database calls.", func() float64 { return float64(in.reg.StoreErrors()) })

	pubStat := func(errors bool) func() float64 {
		return func() float64 {
			if in.pub == nil {
				return 0
			}
			published, errs := in.pub.stats()
			if errors {
				return float64(errs)
			}
			return float64(published)
		}
	}
	metrics.Counter(reg, "murmur_ingester_published_total", "Messages handed to JetStream.", pubStat(false))
	metrics.Counter(reg, "murmur_ingester_publish_errors_total", "Messages JetStream did not accept or acknowledge.", pubStat(true))
}
