package main

import (
	"encoding/json"
	"io"
	"net/netip"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/command"
	"github.com/ChengCK18/murmur/internal/mavlink"
	"github.com/ChengCK18/murmur/internal/registry"
)

type wire struct {
	mu      sync.Mutex
	packets [][]byte
	to      []netip.AddrPort
	replies []command.Reply
}

func (w *wire) send(b []byte, to netip.AddrPort) (int, error) {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.packets = append(w.packets, append([]byte(nil), b...))
	w.to = append(w.to, to)
	return len(b), nil
}

func (w *wire) publish(_ string, data []byte) error {
	var r command.Reply
	if err := json.Unmarshal(data, &r); err != nil {
		return err
	}
	w.mu.Lock()
	defer w.mu.Unlock()
	w.replies = append(w.replies, r)
	return nil
}

var droneAddr = netip.MustParseAddrPort("127.0.0.1:5760")

// flown returns a relay on an ingester that has heard one drone, and its ID.
func flown(t *testing.T) (*relay, *wire, registry.ID) {
	t.Helper()
	in, err := newIngester(time.Second, io.Discard)
	if err != nil {
		t.Fatal(err)
	}
	w := &wire{}
	r, err := newRelay(in, w.send, w.publish)
	if err != nil {
		t.Fatal(err)
	}
	id, ok := in.update(mavlink.Received{
		ReceivedAt: time.Now(), Src: droneAddr, SysID: 7, CompID: 1,
		Msg: &ardupilotmega.MessageHeartbeat{Type: ardupilotmega.MAV_TYPE_QUADROTOR, Autopilot: ardupilotmega.MAV_AUTOPILOT_ARDUPILOTMEGA},
	})
	if !ok {
		t.Fatal("drone not registered")
	}
	return r, w, id
}

func arm(id registry.ID, expires time.Time) []byte {
	b, _ := json.Marshal(command.Command{CommandID: "c1", DroneID: int64(id), Type: command.Arm, Attempt: 2, ExpiresAt: expires})
	return b
}

func TestRelaySends(t *testing.T) {
	r, w, id := flown(t)
	r.handle(arm(id, time.Now().Add(time.Second)))

	if len(w.packets) != 1 || w.to[0] != droneAddr {
		t.Fatalf("sent %d packets to %v", len(w.packets), w.to)
	}
	dec, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	frames, err := dec.Decode(time.Now(), droneAddr, w.packets[0])
	if err != nil || len(frames) != 1 {
		t.Fatalf("%d frames, err %v", len(frames), err)
	}
	f := frames[0]
	if f.SysID != command.SenderSystem || f.CompID != command.SenderComponent {
		t.Errorf("sent as %d/%d", f.SysID, f.CompID)
	}
	m, ok := f.Msg.(*ardupilotmega.MessageCommandLong)
	if !ok {
		t.Fatalf("sent %T", f.Msg)
	}
	if m.TargetSystem != 7 || m.TargetComponent != 1 || m.Param1 != 1 || m.Confirmation != 1 {
		t.Errorf("command %+v", m)
	}
	if len(w.replies) != 1 || w.replies[0].Status != command.Sent || w.replies[0].CommandID != "c1" || w.replies[0].Attempt != 2 {
		t.Errorf("replies %+v", w.replies)
	}
}

func TestRelayNeverSendsAnExpiredCommand(t *testing.T) {
	r, w, id := flown(t)
	r.handle(arm(id, time.Now().Add(-time.Millisecond)))
	if len(w.packets) != 0 {
		t.Error("an expired command reached the drone")
	}
	if len(w.replies) != 1 || w.replies[0].Status != command.Expired {
		t.Errorf("replies %+v", w.replies)
	}
}

func TestRelayRefusesADroneItHasNotHeard(t *testing.T) {
	r, w, id := flown(t)
	r.handle(arm(id+1, time.Now().Add(time.Second)))
	if len(w.packets) != 0 {
		t.Error("sent to a drone with no known address")
	}
	if len(w.replies) != 1 || w.replies[0].Status != command.Unreachable {
		t.Errorf("replies %+v", w.replies)
	}
}

func TestRelayKeepsADroneSequence(t *testing.T) {
	r, w, id := flown(t)
	r.handle(arm(id, time.Now().Add(time.Second)))
	r.handle(arm(id, time.Now().Add(time.Second)))
	dec, err := mavlink.NewDecoder()
	if err != nil {
		t.Fatal(err)
	}
	a, _ := dec.Decode(time.Now(), droneAddr, w.packets[0])
	b, _ := dec.Decode(time.Now(), droneAddr, w.packets[1])
	if b[0].Seq != a[0].Seq+1 {
		t.Errorf("sequence %d then %d", a[0].Seq, b[0].Seq)
	}
}

func TestRelayForwardsAcks(t *testing.T) {
	r, w, id := flown(t)
	r.in.relay = r
	r.in.handle(mavlink.Received{ReceivedAt: time.Now(), Src: droneAddr, SysID: 7, CompID: 1,
		Msg: &ardupilotmega.MessageCommandAck{Command: common.MAV_CMD_COMPONENT_ARM_DISARM, Result: ardupilotmega.MAV_RESULT_DENIED}})
	if len(w.replies) != 1 {
		t.Fatalf("replies %+v", w.replies)
	}
	got := w.replies[0]
	if got.DroneID != int64(id) || got.Ack == nil || got.Ack.Result != uint8(ardupilotmega.MAV_RESULT_DENIED) ||
		got.Ack.Command != uint16(common.MAV_CMD_COMPONENT_ARM_DISARM) {
		t.Errorf("reply %+v, ack %+v", got, got.Ack)
	}
}

func TestRelayCarriesAnUpload(t *testing.T) {
	r, w, id := flown(t)
	r.in.relay = r
	expires := time.Now().Add(time.Second)
	for _, c := range []command.Command{
		command.CountCommand(command.Command{CommandID: "c1", DroneID: int64(id), Attempt: 1, ExpiresAt: expires}, 3),
		command.ItemCommand(command.Command{CommandID: "c1", DroneID: int64(id), Attempt: 2, ExpiresAt: expires}, command.Item{Seq: 1, Command: 16, Frame: 3, Lat: -35, Lon: 149, Alt: 20}),
	} {
		b, _ := json.Marshal(c)
		r.handle(b)
	}
	dec, _ := mavlink.NewDecoder()
	count, _ := dec.Decode(time.Now(), droneAddr, w.packets[0])
	item, _ := dec.Decode(time.Now(), droneAddr, w.packets[1])
	if m, ok := count[0].Msg.(*ardupilotmega.MessageMissionCount); !ok || m.Count != 3 {
		t.Errorf("first packet %+v", count[0].Msg)
	}
	if m, ok := item[0].Msg.(*ardupilotmega.MessageMissionItemInt); !ok || m.Seq != 1 {
		t.Errorf("second packet %+v", item[0].Msg)
	}

	w.replies = nil
	for _, m := range []message.Message{
		&ardupilotmega.MessageMissionRequestInt{Seq: 2},
		&ardupilotmega.MessageMissionRequest{Seq: 1},
		&ardupilotmega.MessageMissionAck{Type: ardupilotmega.MAV_MISSION_NO_SPACE},
	} {
		r.in.handle(mavlink.Received{ReceivedAt: time.Now(), Src: droneAddr, SysID: 7, CompID: 1, Msg: m})
	}
	if len(w.replies) != 3 || w.replies[0].Mission.Seq != 2 || w.replies[1].Mission.Seq != 1 ||
		!w.replies[2].Mission.Ack || w.replies[2].Mission.Result != uint8(ardupilotmega.MAV_MISSION_NO_SPACE) {
		t.Errorf("%+v", w.replies)
	}
}
