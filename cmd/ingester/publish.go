package main

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"

	"github.com/ChengCK18/murmur/internal/bus"
	"github.com/ChengCK18/murmur/internal/schema"
)

// published reports whether a message type is published to NATS: the key
// telemetry plus what alerts and commands need (plan FR-2). The other ~25
// types ArduPilot streams are only used for loss counting.
func published(m message.Message) bool {
	switch m.(type) {
	case *ardupilotmega.MessageHeartbeat,
		*ardupilotmega.MessageGlobalPositionInt,
		*ardupilotmega.MessageAttitude,
		*ardupilotmega.MessageSysStatus,
		*ardupilotmega.MessageBatteryStatus,
		*ardupilotmega.MessageStatustext,
		*ardupilotmega.MessageCommandAck,
		*ardupilotmega.MessageMissionCurrent,
		*ardupilotmega.MessageMissionItemReached:
		return true
	}
	return false
}

// publisher sends telemetry on. The ingester runs without one (console only)
// when -nats is empty.
type publisher interface {
	publish(schema.Telemetry)
	stats() (published, errors uint64)
}

// natsPublisher publishes to JetStream asynchronously, so the UDP read loop
// does not wait for each acknowledgement. At most maxPending publishes are in
// flight; beyond that PublishAsync waits briefly, then fails and the frame is
// counted as a publish error rather than queued without limit.
type natsPublisher struct {
	nc        *nats.Conn
	js        jetstream.JetStream
	published atomic.Uint64
	errors    atomic.Uint64
}

func newNATSPublisher(ctx context.Context, url string, maxPending int) (*natsPublisher, error) {
	nc, err := bus.Connect(url, "murmur-ingester")
	if err != nil {
		return nil, err
	}
	p := &natsPublisher{nc: nc}
	p.js, err = jetstream.New(nc,
		jetstream.WithPublishAsyncMaxPending(maxPending),
		jetstream.WithPublishAsyncErrHandler(func(jetstream.JetStream, *nats.Msg, error) {
			p.errors.Add(1) // no acknowledgement: timeout, or NATS refused it
		}),
	)
	if err == nil {
		_, err = bus.EnsureTelemetryStream(ctx, p.js)
	}
	if err != nil {
		nc.Close()
		return nil, err
	}
	return p, nil
}

func (p *natsPublisher) publish(t schema.Telemetry) {
	data, err := json.Marshal(t)
	if err == nil {
		_, err = p.js.PublishAsync(t.Subject(), data, jetstream.WithMsgID(t.DedupID()))
	}
	if err != nil {
		p.errors.Add(1)
		return
	}
	p.published.Add(1)
}

func (p *natsPublisher) stats() (uint64, uint64) {
	return p.published.Load(), p.errors.Load()
}

// close waits up to timeout for outstanding acknowledgements, then closes the
// connection.
func (p *natsPublisher) close(timeout time.Duration) {
	select {
	case <-p.js.PublishAsyncComplete():
	case <-time.After(timeout):
	}
	p.nc.Close()
}
