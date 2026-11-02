// Package bus sets up NATS and JetStream for Murmur's services.
//
// Telemetry goes through the TELEMETRY stream (telemetry.<drone_id>), so a
// consumer that restarts carries on where it stopped. Commands (command.*,
// command_reply.*) stay on core NATS, outside any stream (plan §4): a command
// that cannot be delivered now must fail, not run later.
package bus

import (
	"context"
	"fmt"
	"time"

	"github.com/nats-io/nats.go"
	"github.com/nats-io/nats.go/jetstream"
)

const (
	TelemetryStream   = "TELEMETRY"
	TelemetrySubjects = "telemetry.>"
)

// TelemetryStreamConfig is the TELEMETRY stream (guide step 2.3). Ten minutes
// of history covers a consumer restart; long-term storage is TimescaleDB.
func TelemetryStreamConfig() jetstream.StreamConfig {
	return jetstream.StreamConfig{
		Name:       TelemetryStream,
		Subjects:   []string{TelemetrySubjects},
		Storage:    jetstream.FileStorage,
		MaxAge:     10 * time.Minute,
		Duplicates: 2 * time.Minute, // window for Nats-Msg-Id de-duplication
	}
}

// Connect connects to NATS and keeps reconnecting for as long as the
// connection is open, so a NATS restart does not stop the service. name shows
// in NATS monitoring.
func Connect(url, name string) (*nats.Conn, error) {
	nc, err := nats.Connect(url,
		nats.Name(name),
		nats.MaxReconnects(-1),
		nats.ReconnectWait(time.Second),
	)
	if err != nil {
		return nil, fmt.Errorf("bus: connect to %s: %w", url, err)
	}
	return nc, nil
}

// EnsureTelemetryStream creates the TELEMETRY stream, or updates it to the
// current configuration if it already exists.
func EnsureTelemetryStream(ctx context.Context, js jetstream.JetStream) (jetstream.Stream, error) {
	s, err := js.CreateOrUpdateStream(ctx, TelemetryStreamConfig())
	if err != nil {
		return nil, fmt.Errorf("bus: stream %s: %w", TelemetryStream, err)
	}
	return s, nil
}
