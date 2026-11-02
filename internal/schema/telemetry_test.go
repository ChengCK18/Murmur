package schema

import (
	"encoding/json"
	"fmt"
	"math"
	"net/netip"
	"strings"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/tlog"
)

func decodeFile(t *testing.T, name string) []mavlink.Received {
	t.Helper()
	recs, err := tlog.ReadFile("../../testdata/" + name)
	if err != nil {
		t.Fatal(err)
	}
	d, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	var out []mavlink.Received
	for _, rec := range recs {
		frames, err := d.Decode(rec.Time, netip.AddrPort{}, rec.Frame)
		if err != nil {
			t.Fatal(err)
		}
		out = append(out, frames...)
	}
	return out
}

// roundTrip encodes r as it would be published and decodes it as a consumer would.
func roundTrip(t *testing.T, r mavlink.Received) (Telemetry, []byte) {
	t.Helper()
	tel, err := FromReceived(1, 42, r)
	if err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(tel)
	if err != nil {
		t.Fatal(err)
	}
	var got Telemetry
	if err := json.Unmarshal(data, &got); err != nil {
		t.Fatal(err)
	}
	return got, data
}

func TestRoundTripEveryMessage(t *testing.T) {
	// Every frame in the flight: the 5 key types and the ~29 others.
	types := map[string]bool{}
	for _, r := range decodeFile(t, "flight.tlog") {
		got, data := roundTrip(t, r)
		msg, err := got.Message()
		if err != nil {
			t.Fatalf("%s: %v", data, err)
		}
		// %#v rather than reflect.DeepEqual, because NaN != NaN.
		if fmt.Sprintf("%#v", msg) != fmt.Sprintf("%#v", r.Msg) {
			t.Fatalf("%s round trip\n got %+v\nwant %+v", got.Msg, msg, r.Msg)
		}
		if got.SysID != r.SysID || got.CompID != r.CompID || got.Seq != r.Seq || !got.ReceivedAt.Equal(r.ReceivedAt) {
			t.Fatalf("%s header changed: %s", got.Msg, data)
		}
		types[got.Msg] = true
	}
	for _, k := range []string{"HEARTBEAT", "ATTITUDE", "GLOBAL_POSITION_INT", "SYS_STATUS", "BATTERY_STATUS", "STATUSTEXT"} {
		if !types[k] {
			t.Errorf("no %s in the recording", k)
		}
	}
	t.Logf("%d message types round-tripped", len(types))
}

func TestGlobalPositionIntJSON(t *testing.T) {
	// The example from guide step 2.2, with values from flight.tlog.
	r := mavlink.Received{
		ReceivedAt: time.Date(2026, 11, 1, 10, 0, 0, 123456789, time.UTC),
		SysID:      1, CompID: 1, Seq: 17,
		Msg: &ardupilotmega.MessageGlobalPositionInt{
			TimeBootMs: 58609, Lat: -353632348, Lon: 1491652368, Alt: 604050, RelativeAlt: 19972,
			Vx: 311, Vy: 2, Vz: -3, Hdg: 35994,
		},
	}
	tel, err := FromReceived(1, 42, r)
	if err != nil {
		t.Fatal(err)
	}
	data, _ := json.Marshal(tel)
	want := `{"tenant_id":1,"drone_id":42,"sys_id":1,"comp_id":1,"seq":17,"msg_id":33,"msg":"GLOBAL_POSITION_INT",` +
		`"received_at":"2026-11-01T10:00:00.123456789Z","time_boot_ms":58609,` +
		`"payload":{"alt":604050,"hdg":35994,"lat":-353632348,"lon":1491652368,"relative_alt":19972,"time_boot_ms":58609,"vx":311,"vy":2,"vz":-3}}`
	if string(data) != want {
		t.Errorf("JSON\n got %s\nwant %s", data, want)
	}
	if s := tel.Subject(); s != "telemetry.42" {
		t.Errorf("subject %q", s)
	}
	if id := tel.DedupID(); id != "42:GLOBAL_POSITION_INT:1793527200123456789" {
		t.Errorf("dedup ID %q", id)
	}
}

func TestPayloadValues(t *testing.T) {
	hb := &ardupilotmega.MessageHeartbeat{
		Type: ardupilotmega.MAV_TYPE_QUADROTOR, Autopilot: ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA,
		BaseMode: 217, CustomMode: 4, SystemStatus: ardupilotmega.MAV_STATE_ACTIVE, MavlinkVersion: 3,
	}
	tel, err := FromReceived(1, 1, mavlink.Received{Msg: hb})
	if err != nil {
		t.Fatal(err)
	}
	// Enums as numbers, not gomavlib's names; no time_boot_ms for a heartbeat.
	want := `{"autopilot":3,"base_mode":217,"custom_mode":4,"mavlink_version":3,"system_status":4,"type":2}`
	if string(tel.Payload) != want || tel.TimeBootMs != nil {
		t.Errorf("payload %s time_boot_ms %v\nwant %s and none", tel.Payload, tel.TimeBootMs, want)
	}

	// float32 without float64 noise; arrays; strings.
	att, _ := FromReceived(1, 1, mavlink.Received{Msg: &ardupilotmega.MessageAttitude{Roll: 0.0041}})
	if !strings.Contains(string(att.Payload), `"roll":0.0041,`) {
		t.Errorf("attitude payload %s", att.Payload)
	}
	bat, _ := FromReceived(1, 1, mavlink.Received{Msg: &ardupilotmega.MessageBatteryStatus{Voltages: [10]uint16{12462, 65535}}})
	if !strings.Contains(string(bat.Payload), `"voltages":[12462,65535,0,0,0,0,0,0,0,0]`) {
		t.Errorf("battery payload %s", bat.Payload)
	}
	// NaN, which JSON cannot hold, travels as null and comes back as NaN.
	home, _ := FromReceived(1, 1, mavlink.Received{Msg: &ardupilotmega.MessageHomePosition{
		Q: [4]float32{float32(math.NaN()), 1, float32(math.Inf(1)), 0}}})
	if !strings.Contains(string(home.Payload), `"q":[null,1,null,0]`) {
		t.Errorf("home payload %s", home.Payload)
	}
	if m, err := home.Message(); err != nil || !math.IsNaN(float64(m.(*ardupilotmega.MessageHomePosition).Q[0])) {
		t.Errorf("null did not come back as NaN: %v %v", m, err)
	}

	txt, _ := FromReceived(1, 1, mavlink.Received{Msg: &ardupilotmega.MessageStatustext{Text: "Arming motors"}})
	if !strings.Contains(string(txt.Payload), `"text":"Arming motors"`) || txt.Msg != "STATUSTEXT" {
		t.Errorf("statustext %s %s", txt.Msg, txt.Payload)
	}
}

func TestBadPayload(t *testing.T) {
	good, _ := FromReceived(1, 1, mavlink.Received{Msg: &ardupilotmega.MessageAttitude{}})
	tests := []struct {
		name    string
		msgID   uint32
		payload string
	}{
		{"unknown message ID", 999999, `{}`},
		{"not JSON", 30, `{`},
		{"string for a number", 30, `{"roll":"x"}`},
		{"out of range", 0, `{"type":300}`},
		{"wrong array length", 147, `{"voltages":[1,2]}`},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			bad := good
			bad.MsgID, bad.Payload = tt.msgID, json.RawMessage(tt.payload)
			if _, err := bad.Message(); err == nil {
				t.Error("no error")
			}
		})
	}
}
