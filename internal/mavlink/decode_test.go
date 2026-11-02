package mavlink

import (
	"errors"
	"fmt"
	"net/netip"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/tlog"
)

func newDecoder(t *testing.T) *Decoder {
	t.Helper()
	d, err := NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// decodeFile decodes every record of a recording, failing on any error.
func decodeFile(t *testing.T, name string) []Received {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d := newDecoder(t)
	var out []Received
	for i, rec := range recs {
		got, err := d.Decode(rec.Time, netip.AddrPort{}, rec.Frame)
		if err != nil {
			t.Fatalf("record %d: %v", i, err)
		}
		if len(got) != 1 {
			t.Fatalf("record %d: %d frames, want 1", i, len(got))
		}
		if !got[0].ReceivedAt.Equal(rec.Time) {
			t.Fatalf("record %d: ReceivedAt %v, want %v", i, got[0].ReceivedAt, rec.Time)
		}
		out = append(out, got[0])
	}
	return out
}

func TestDecodeRecordings(t *testing.T) {
	// Every frame decodes, including the ~25 message types Murmur does not
	// publish: their sequence numbers still count for loss detection.
	for _, name := range []string{"idle.tlog", "flight.tlog", "restart.tlog"} {
		t.Run(name, func(t *testing.T) {
			for i, r := range decodeFile(t, name) {
				if raw, ok := r.Msg.(*message.MessageRaw); ok {
					t.Errorf("frame %d: message ID %d not in the dialect", i, raw.ID)
				}
			}
		})
	}
}

func TestKeyMessageCounts(t *testing.T) {
	// Cross-checked with pymavlink on the same file.
	want := map[string]int{
		"1 HEARTBEAT":           165,
		"255 HEARTBEAT":         156,
		"1 SYS_STATUS":          317,
		"1 ATTITUDE":            793,
		"1 GLOBAL_POSITION_INT": 793,
		"1 BATTERY_STATUS":      158,
	}
	got := map[string]int{}
	for _, r := range decodeFile(t, "flight.tlog") {
		var name string
		switch r.Msg.(type) {
		case *ardupilotmega.MessageHeartbeat:
			name = "HEARTBEAT"
		case *ardupilotmega.MessageSysStatus:
			name = "SYS_STATUS"
		case *ardupilotmega.MessageAttitude:
			name = "ATTITUDE"
		case *ardupilotmega.MessageGlobalPositionInt:
			name = "GLOBAL_POSITION_INT"
		case *ardupilotmega.MessageBatteryStatus:
			name = "BATTERY_STATUS"
		default:
			continue
		}
		got[fmt.Sprintf("%d %s", r.SysID, name)]++
	}
	for k, n := range want {
		if got[k] != n {
			t.Errorf("%s: %d, want %d", k, got[k], n)
		}
	}
	if len(got) != len(want) {
		t.Errorf("unexpected senders or types: %v", got)
	}
}

func TestDecodedValues(t *testing.T) {
	frames := decodeFile(t, "flight.tlog")

	// The first frame: the drone's boot HEARTBEAT (local/testdata-guide.md, section 2).
	first := frames[0]
	hb, ok := first.Msg.(*ardupilotmega.MessageHeartbeat)
	if !ok {
		t.Fatalf("first frame is %T, want HEARTBEAT", first.Msg)
	}
	if first.SysID != 1 || first.CompID != 1 || first.Seq != 0 {
		t.Errorf("header sys %d comp %d seq %d, want 1 1 0", first.SysID, first.CompID, first.Seq)
	}
	if hb.Type != ardupilotmega.MAV_TYPE_QUADROTOR || hb.Autopilot != ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA ||
		hb.BaseMode != 89 || hb.CustomMode != 0 || hb.SystemStatus != ardupilotmega.MAV_STATE_BOOT {
		t.Errorf("first heartbeat = %+v", *hb)
	}

	// A position on the first leg of the mission (local/mavlink-notes.md, GLOBAL_POSITION_INT).
	var gpi *ardupilotmega.MessageGlobalPositionInt
	for _, r := range frames {
		if m, ok := r.Msg.(*ardupilotmega.MessageGlobalPositionInt); ok && m.TimeBootMs == 58609 {
			gpi = m
			break
		}
	}
	if gpi == nil {
		t.Fatal("GLOBAL_POSITION_INT at time_boot_ms 58609 not found")
	}
	want := ardupilotmega.MessageGlobalPositionInt{
		TimeBootMs: 58609, Lat: -353632348, Lon: 1491652368, Alt: 604050, RelativeAlt: 19972,
		Vx: 311, Vy: 2, Vz: -3, Hdg: 35994,
	}
	if *gpi != want {
		t.Errorf("GLOBAL_POSITION_INT = %+v\nwant %+v", *gpi, want)
	}

	// The drone's flight modes in order, and armed only while flying.
	var modes []uint32
	armedSeen := false
	for _, r := range frames {
		hb, ok := r.Msg.(*ardupilotmega.MessageHeartbeat)
		if !ok || r.SysID != 1 {
			continue
		}
		if len(modes) == 0 || modes[len(modes)-1] != hb.CustomMode {
			modes = append(modes, hb.CustomMode)
		}
		armedSeen = armedSeen || hb.BaseMode&ardupilotmega.MAV_MODE_FLAG_SAFETY_ARMED != 0
	}
	if fmt.Sprint(modes) != "[0 4 3]" { // STABILIZE, GUIDED, AUTO
		t.Errorf("modes %v, want [0 4 3]", modes)
	}
	if !armedSeen {
		t.Error("no armed heartbeat")
	}
}

func TestDecodePackets(t *testing.T) {
	recs, err := tlog.ReadFile("../../testdata/flight.tlog")
	if err != nil {
		t.Fatal(err)
	}
	hb := recs[0].Frame // drone HEARTBEAT
	mc := recs[1].Frame // ground-station MISSION_COUNT

	corrupt := append([]byte(nil), hb...)
	corrupt[14] ^= 0xFF // a payload byte: the checksum no longer matches

	cat := func(parts ...[]byte) []byte {
		var b []byte
		for _, p := range parts {
			b = append(b, p...)
		}
		return b
	}

	tests := []struct {
		name    string
		data    []byte
		frames  int
		wantErr bool
	}{
		{"one frame", hb, 1, false},
		{"two frames in one datagram", cat(hb, mc), 2, false},
		{"empty", nil, 0, false},
		// The first datagrams SITL sends, captured live (finding 3).
		{"SITL boot text", []byte("\n\nInit ArduCopter V4.8.0-dev (9165d224)\n\nFree RAM: 524288\n"), 0, true},
		{"SITL boot text fragment", []byte("0 "), 0, true},
		{"bad checksum", corrupt, 0, true},
		{"truncated frame", hb[:len(hb)-3], 0, true},
		{"frame then junk", cat(hb, []byte("junk")), 1, true},
		{"junk then frame", cat([]byte("junk"), hb), 0, true}, // dropped: boundaries after junk are not trusted
	}
	d := newDecoder(t)
	src := netip.MustParseAddrPort("127.0.0.1:42318")
	at := time.Unix(1793516806, 0)
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := d.Decode(at, src, tt.data)
			if len(got) != tt.frames {
				t.Errorf("%d frames, want %d", len(got), tt.frames)
			}
			if tt.wantErr != (err != nil) {
				t.Errorf("err = %v, want error: %v", err, tt.wantErr)
			}
			if err != nil && !errors.Is(err, ErrDecode) {
				t.Errorf("err = %v, does not wrap ErrDecode", err)
			}
			for _, r := range got {
				if r.Src != src || !r.ReceivedAt.Equal(at) {
					t.Errorf("Src %v ReceivedAt %v, want %v %v", r.Src, r.ReceivedAt, src, at)
				}
			}
		})
	}
}
