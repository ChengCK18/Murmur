package main

import (
	"context"
	"sync/atomic"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/ChengCK18/murmur/internal/metrics"
)

// observers are the consumer's histograms (guide step 2.7).
type observers struct {
	batchSize prometheus.Histogram
	writeTime prometheus.Histogram
	// latency is receive to commit: from the ingester receiving a frame
	// (received_at) to the database commit that stored it. NFR-2 reports its
	// p50 and p99.
	latency prometheus.Histogram
	lag     atomic.Int64 // pending plus unacknowledged, refreshed by trackLag
}

func newObservers(reg prometheus.Registerer, c *consumer) *observers {
	o := &observers{
		batchSize: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "murmur_consumer_batch_size",
			Help:    "Messages per batch written.",
			Buckets: []float64{1, 5, 10, 25, 50, 100, 250, 500, 1000},
		}),
		writeTime: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "murmur_consumer_batch_write_seconds",
			Help:    "Time to write a batch to the database and Redis.",
			Buckets: metrics.LatencyBuckets,
		}),
		latency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "murmur_consumer_receive_to_commit_seconds",
			Help:    "From the ingester receiving a frame to the database commit that stored it.",
			Buckets: metrics.LatencyBuckets,
		}),
	}
	reg.MustRegister(o.batchSize, o.writeTime, o.latency)

	s := &c.stats
	count := func(v *atomic.Uint64) func() float64 { return func() float64 { return float64(v.Load()) } }
	metrics.Counter(reg, "murmur_consumer_fetched_total", "Messages read from the stream.", count(&s.fetched))
	metrics.Counter(reg, "murmur_consumer_inserted_total", "Rows new to the database.", count(&s.inserted))
	metrics.Counter(reg, "murmur_consumer_duplicates_total", "Rows already stored: redeliveries.", count(&s.duplicates))
	metrics.Counter(reg, "murmur_consumer_rejected_total", "Messages that are not valid telemetry.", count(&s.rejected))
	metrics.Counter(reg, "murmur_consumer_failed_batches_total", "Batches not written, left for redelivery.", count(&s.failed))
	metrics.Gauge(reg, "murmur_consumer_lag", "Messages not yet acknowledged: pending in the stream plus delivered but unacknowledged.",
		func() float64 { return float64(o.lag.Load()) })
	return o
}

// observe records one batch written at commit.
func (o *observers) observe(b batch, took time.Duration, commit time.Time) {
	if o == nil {
		return
	}
	o.batchSize.Observe(float64(len(b.tels)))
	o.writeTime.Observe(took.Seconds())
	for _, t := range b.tels {
		o.latency.Observe(commit.Sub(t.ReceivedAt).Seconds())
	}
}

// trackLag refreshes the lag gauge every interval, so a scrape never waits on
// NATS.
func (o *observers) trackLag(ctx context.Context, c *consumer, every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		if info, err := c.cons.Info(ctx); err == nil {
			o.lag.Store(int64(info.NumPending) + int64(info.NumAckPending))
		}
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		}
	}
}
