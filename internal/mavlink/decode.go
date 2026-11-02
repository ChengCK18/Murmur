// Package mavlink decodes MAVLink frames into Murmur's own Received type, so
// the rest of the code does not depend on gomavlib's frame types.
package mavlink

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"net/netip"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialect"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/frame"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

// Received is one decoded frame and where and when it arrived.
type Received struct {
	ReceivedAt time.Time
	Src        netip.AddrPort // UDP sender; the zero value when decoded from a .tlog
	SysID      uint8
	CompID     uint8
	Seq        uint8
	// Msg is a typed message such as *ardupilotmega.MessageHeartbeat, or a
	// *message.MessageRaw if the message ID is not in the ardupilotmega dialect.
	Msg message.Message
}

// ErrDecode wraps every error caused by bytes that are not a valid frame.
var ErrDecode = errors.New("mavlink: decode error")

// Decoder decodes frames using the ardupilotmega dialect, which includes the
// common, standard and minimal dialects.
type Decoder struct {
	rw *dialect.ReadWriter
}

// NewDecoder returns a Decoder for the ardupilotmega dialect.
func NewDecoder() (*Decoder, error) {
	rw, err := dialect.NewReadWriter(ardupilotmega.Dialect)
	if err != nil {
		return nil, fmt.Errorf("mavlink: dialect: %w", err)
	}
	return &Decoder{rw: rw}, nil
}

// Decode decodes the frames in data: one UDP datagram, or one .tlog record.
//
// SITL sends exactly one frame per datagram, but any number is accepted. If
// bytes that are not a valid frame are found (SITL's boot text, a bad
// checksum, a truncated frame), Decode returns the frames decoded before them
// and an error wrapping ErrDecode, and drops the rest of data: after bad bytes
// the frame boundaries cannot be trusted. A bad datagram therefore counts as
// one error, however long it is.
func (d *Decoder) Decode(at time.Time, src netip.AddrPort, data []byte) ([]Received, error) {
	r, err := frame.NewReader(frame.ReaderConf{Reader: bytes.NewReader(data), DialectRW: d.rw})
	if err != nil {
		return nil, fmt.Errorf("mavlink: reader: %w", err)
	}
	var out []Received
	for {
		f, err := r.Read()
		if err == io.EOF {
			return out, nil
		}
		if err != nil {
			return out, fmt.Errorf("%w: %v", ErrDecode, err)
		}
		out = append(out, Received{
			ReceivedAt: at,
			Src:        src,
			SysID:      f.GetSystemID(),
			CompID:     f.GetComponentID(),
			Seq:        f.GetSequenceNumber(),
			Msg:        f.GetMessage(),
		})
	}
}
