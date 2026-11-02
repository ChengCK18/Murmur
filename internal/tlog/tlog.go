// Package tlog reads MAVProxy telemetry logs (.tlog files).
//
// A .tlog is a sequence of records with nothing between them. Each record is
// an 8-byte big-endian timestamp (microseconds since 1970) followed by one
// complete MAVLink v1 or v2 frame, exactly as it arrived on the wire. Frames
// are returned as raw bytes; decoding them is the job of internal/mavlink.
package tlog

import (
	"bufio"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"os"
	"time"
)

// MAVLink frame layout, used to find where each frame ends.
const (
	magicV1 = 0xFE
	magicV2 = 0xFD

	headerLenV1    = 6  // magic, len, seq, sysid, compid, msgid
	headerLenV2    = 10 // magic, len, incompat, compat, seq, sysid, compid, msgid (3 bytes)
	checksumLen    = 2
	signatureLen   = 13
	incompatSigned = 0x01 // MAVLINK_IFLAG_SIGNED: the frame carries a signature
)

// ErrBadMagic means a record's frame does not start with a MAVLink start
// byte, so the file is corrupt or is not a .tlog. Reading cannot continue,
// because without a valid header the frame length is unknown.
var ErrBadMagic = errors.New("tlog: frame does not start with a MAVLink magic byte")

// Record is one entry of a .tlog file.
type Record struct {
	Time  time.Time // when the frame was received
	Frame []byte    // one complete MAVLink frame, including header and checksum
}

// Reader reads records from a .tlog stream.
type Reader struct {
	r      *bufio.Reader
	offset int64 // byte offset of the next record, for error messages
}

// NewReader returns a Reader that reads records from r.
func NewReader(r io.Reader) *Reader {
	return &Reader{r: bufio.NewReader(r)}
}

// Read returns the next record. It returns io.EOF when the stream ends cleanly
// between records, and io.ErrUnexpectedEOF (wrapped) when it ends partway
// through one, as happens when the recorder is killed mid-write.
func (r *Reader) Read() (Record, error) {
	var ts [8]byte
	n, err := io.ReadFull(r.r, ts[:])
	if err == io.EOF {
		return Record{}, io.EOF
	}
	if err != nil {
		return Record{}, r.errorf(n, "timestamp: %w", err)
	}

	head, err := r.r.Peek(3)
	if err != nil {
		return Record{}, r.errorf(8, "frame header: %w", unexpected(err))
	}
	size, err := frameLen(head)
	if err != nil {
		return Record{}, r.errorf(8, "%w", err)
	}

	frame := make([]byte, size)
	n, err = io.ReadFull(r.r, frame)
	if err != nil {
		return Record{}, r.errorf(8+n, "frame of %d bytes: %w", size, unexpected(err))
	}

	usec := binary.BigEndian.Uint64(ts[:])
	r.offset += int64(8 + size)
	return Record{Time: time.UnixMicro(int64(usec)), Frame: frame}, nil
}

// frameLen returns the total length of the frame whose first bytes are head.
func frameLen(head []byte) (int, error) {
	payload := int(head[1])
	switch head[0] {
	case magicV1:
		return headerLenV1 + payload + checksumLen, nil
	case magicV2:
		n := headerLenV2 + payload + checksumLen
		if head[2]&incompatSigned != 0 {
			n += signatureLen
		}
		return n, nil
	}
	return 0, fmt.Errorf("%w (got 0x%02X)", ErrBadMagic, head[0])
}

func (r *Reader) errorf(consumed int, format string, args ...any) error {
	err := fmt.Errorf("tlog: record at byte %d: "+format, append([]any{r.offset}, args...)...)
	r.offset += int64(consumed)
	return err
}

// unexpected turns io.EOF in the middle of a record into io.ErrUnexpectedEOF.
func unexpected(err error) error {
	if err == io.EOF {
		return io.ErrUnexpectedEOF
	}
	return err
}

// ReadFile reads every record in the .tlog file at path.
func ReadFile(path string) ([]Record, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer f.Close()

	var recs []Record
	r := NewReader(f)
	for {
		rec, err := r.Read()
		if err == io.EOF {
			return recs, nil
		}
		if err != nil {
			return recs, err
		}
		recs = append(recs, rec)
	}
}
