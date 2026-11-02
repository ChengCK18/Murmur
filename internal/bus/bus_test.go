package bus

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/nats-io/nats.go/jetstream"

	"github.com/ChengCK18/murmur/internal/bus/natstest"
)

func setup(t *testing.T) (context.Context, jetstream.JetStream) {
	t.Helper()
	nc, err := Connect(natstest.Start(t), "bus-test")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(nc.Close)
	js, err := jetstream.New(nc)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	t.Cleanup(cancel)
	return ctx, js
}

func TestEnsureTelemetryStream(t *testing.T) {
	ctx, js := setup(t)

	// An older configuration is brought up to date.
	old := TelemetryStreamConfig()
	old.MaxAge = time.Hour
	if _, err := js.CreateStream(ctx, old); err != nil {
		t.Fatal(err)
	}
	for range 2 { // and running it again is harmless
		if _, err := EnsureTelemetryStream(ctx, js); err != nil {
			t.Fatal(err)
		}
	}
	s, err := js.Stream(ctx, TelemetryStream)
	if err != nil {
		t.Fatal(err)
	}
	cfg := s.CachedInfo().Config
	if cfg.MaxAge != 10*time.Minute || cfg.Duplicates != 2*time.Minute ||
		cfg.Storage != jetstream.FileStorage || len(cfg.Subjects) != 1 || cfg.Subjects[0] != "telemetry.>" {
		t.Errorf("config %+v", cfg)
	}
}

func TestDuplicatesDropped(t *testing.T) {
	ctx, js := setup(t)
	s, err := EnsureTelemetryStream(ctx, js)
	if err != nil {
		t.Fatal(err)
	}
	// The same Nats-Msg-Id twice, as when a publish is retried after a timeout.
	for i := range 2 {
		ack, err := js.Publish(ctx, "telemetry.42", []byte(`{}`), jetstream.WithMsgID("42:HEARTBEAT:1"))
		if err != nil {
			t.Fatal(err)
		}
		if ack.Duplicate != (i == 1) {
			t.Errorf("publish %d: duplicate %v", i, ack.Duplicate)
		}
	}
	if _, err := js.Publish(ctx, "telemetry.42", []byte(`{}`), jetstream.WithMsgID("42:HEARTBEAT:2")); err != nil {
		t.Fatal(err)
	}
	info, err := s.Info(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if info.State.Msgs != 2 {
		t.Errorf("%d messages stored, want 2", info.State.Msgs)
	}
}

func TestCommandsNotInStream(t *testing.T) {
	ctx, js := setup(t)
	if _, err := EnsureTelemetryStream(ctx, js); err != nil {
		t.Fatal(err)
	}
	_, err := js.Publish(ctx, "command.42", []byte(`{}`))
	if !errors.Is(err, jetstream.ErrNoStreamResponse) {
		t.Errorf("publishing a command to JetStream: %v, want no stream", err)
	}
}
