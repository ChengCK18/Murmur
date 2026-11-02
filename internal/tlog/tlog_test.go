package tlog

import (
	"bytes"
	"encoding/binary"
	"errors"
	"io"
	"testing"
	"time"
)

func TestReadRecordings(t *testing.T) {
	// Frame counts cross-checked with pymavlink (mavutil, all non-BAD_DATA messages).
	tests := []struct {
		file   string
		frames int
	}{
		{"idle.tlog", 5906},
		{"flight.tlog", 8180},
		{"restart.tlog", 10122},
	}
	for _, tt := range tests {
		t.Run(tt.file, func(t *testing.T) {
			recs, err := ReadFile("../../testdata/" + tt.file)
			if err != nil {
				t.Fatal(err)
			}
			if len(recs) != tt.frames {
				t.Errorf("got %d records, want %d", len(recs), tt.frames)
			}
			senders := map[uint8]int{}
			for i, rec := range recs {
				if rec.Frame[0] != magicV2 {
					t.Fatalf("record %d: not a MAVLink 2 frame (0x%02X)", i, rec.Frame[0])
				}
				if i > 0 && rec.Time.Before(recs[i-1].Time) {
					t.Fatalf("record %d: time goes backwards (%v after %v)", i, rec.Time, recs[i-1].Time)
				}
				senders[rec.Frame[5]]++
			}
			// Only the drone (system ID 1) and the recorder acting as ground station (255).
			if len(senders) != 2 || senders[1] == 0 || senders[255] == 0 {
				t.Errorf("unexpected system IDs: %v", senders)
			}
		})
	}
}

func TestFirstRecordOfFlight(t *testing.T) {
	recs, err := ReadFile("../../testdata/flight.tlog")
	if err != nil {
		t.Fatal(err)
	}
	// The HEARTBEAT taken apart byte by byte in local/testdata-guide.md.
	want := []byte{
		0xFD, 0x09, 0x00, 0x00, 0x00, 0x01, 0x01, 0x00, 0x00, 0x00, // header: len 9, seq 0, sys 1, comp 1, msg 0
		0x00, 0x00, 0x00, 0x00, 0x02, 0x03, 0x59, 0x01, 0x03, // payload
		0x82, 0xC2, // checksum
	}
	if !bytes.Equal(recs[0].Frame, want) {
		t.Errorf("frame = % X\nwant    % X", recs[0].Frame, want)
	}
	if got := recs[0].Time.UnixMicro(); got != 1793516806995375 {
		t.Errorf("time = %d µs, want 1793516806995375", got)
	}
}

// record builds one .tlog record from a timestamp and frame bytes.
func record(usec uint64, frame ...byte) []byte {
	b := binary.BigEndian.AppendUint64(nil, usec)
	return append(b, frame...)
}

// v2 builds a MAVLink 2 frame with the given incompat flags and payload length.
// Only the layout matters to this package, so header fields and checksum are zero.
func v2(incompat byte, payload int) []byte {
	n := headerLenV2 + payload + checksumLen
	if incompat&incompatSigned != 0 {
		n += signatureLen
	}
	f := make([]byte, n)
	f[0], f[1], f[2] = magicV2, byte(payload), incompat
	return f
}

func v1(payload int) []byte {
	f := make([]byte, headerLenV1+payload+checksumLen)
	f[0], f[1] = magicV1, byte(payload)
	return f
}

func TestFrameKinds(t *testing.T) {
	tests := []struct {
		name  string
		frame []byte
		size  int
	}{
		{"mavlink1", v1(9), 17},
		{"mavlink2", v2(0, 9), 21},
		{"mavlink2 signed", v2(incompatSigned, 9), 34},
		{"mavlink2 empty payload", v2(0, 0), 12},
		{"mavlink2 max payload", v2(0, 255), 267},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Two records back to back: the second only parses if the first frame's length was right.
			data := append(record(1, tt.frame...), record(2, tt.frame...)...)
			r := NewReader(bytes.NewReader(data))
			for i := range 2 {
				rec, err := r.Read()
				if err != nil {
					t.Fatalf("record %d: %v", i, err)
				}
				if len(rec.Frame) != tt.size {
					t.Errorf("record %d: frame length %d, want %d", i, len(rec.Frame), tt.size)
				}
				if want := time.UnixMicro(int64(i + 1)); !rec.Time.Equal(want) {
					t.Errorf("record %d: time %v, want %v", i, rec.Time, want)
				}
			}
			if _, err := r.Read(); err != io.EOF {
				t.Errorf("after last record: err = %v, want io.EOF", err)
			}
		})
	}
}

func TestBadInput(t *testing.T) {
	good := record(1, v2(0, 9)...)
	tests := []struct {
		name string
		data []byte
		want error
	}{
		{"empty", nil, io.EOF},
		{"truncated timestamp", good[:5], io.ErrUnexpectedEOF},
		{"timestamp only", good[:8], io.ErrUnexpectedEOF},
		{"truncated header", good[:9], io.ErrUnexpectedEOF},
		{"truncated frame", good[:len(good)-1], io.ErrUnexpectedEOF},
		{"bad magic", record(1, 'A', 'P', ':', ' ', 'b', 'o', 'o', 't'), ErrBadMagic},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := NewReader(bytes.NewReader(tt.data)).Read()
			if !errors.Is(err, tt.want) {
				t.Errorf("err = %v, want %v", err, tt.want)
			}
		})
	}
}

func TestErrorAfterGoodRecords(t *testing.T) {
	// A recorder killed mid-write: the complete records are still returned.
	data := append(record(1, v2(0, 9)...), record(2, v2(0, 9)...)...)
	data = data[:len(data)-3]
	recs, err := readAll(NewReader(bytes.NewReader(data)))
	if len(recs) != 1 || !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatalf("got %d records, err %v; want 1 record and io.ErrUnexpectedEOF", len(recs), err)
	}
	want := "tlog: record at byte 29: "
	if got := err.Error(); len(got) < len(want) || got[:len(want)] != want {
		t.Errorf("error %q does not report the offset of the bad record (%q)", got, want)
	}
}

func readAll(r *Reader) ([]Record, error) {
	var recs []Record
	for {
		rec, err := r.Read()
		if err != nil {
			return recs, err
		}
		recs = append(recs, rec)
	}
}
