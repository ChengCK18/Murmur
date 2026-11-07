package main

import (
	"context"
	"testing"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"

	"github.com/ChengCK18/murmur/internal/command"
)

func missionReq() command.MissionRequest {
	return command.MissionRequest{DroneID: 42, AltitudeM: 20, IssuedBy: "demo",
		Waypoints: []command.Waypoint{{Lat: -35.36, Lon: 149.16}, {Lat: -35.37, Lon: 149.17}}}
}

// drone plays a drone: it asks for each item in turn, then accepts.
func (h *harness) playDrone(c command.Command) {
	h.s.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: command.Sent})
	switch {
	case c.Params.Count > 0:
		h.s.reply(command.Reply{DroneID: c.DroneID, Mission: &command.MissionEvent{Seq: 0}})
	case c.Params.Item != nil:
		next := c.Params.Item.Seq + 1
		if next == 5 {
			h.s.reply(command.Reply{DroneID: c.DroneID, Mission: &command.MissionEvent{Ack: true}})
			return
		}
		h.s.reply(command.Reply{DroneID: c.DroneID, Mission: &command.MissionEvent{Seq: next}})
	}
}

func TestUploadSendsEveryItem(t *testing.T) {
	h := newHarness(t, fast)
	h.onSend = h.playDrone
	if u := h.s.submitMission(context.Background(), missionReq()); u.Status != command.Pending {
		t.Fatalf("%+v", u)
	}
	if u := h.final(); u.Status != command.Accepted || u.Type != command.Upload {
		t.Fatalf("%+v", u)
	}
	sends := h.sends()
	if len(sends) != 6 || sends[0].Params.Count != 5 { // count, then home, takeoff, 2 waypoints, RTL
		t.Fatalf("%d packets, first %+v", len(sends), sends[0])
	}
	for i, c := range sends[1:] {
		if c.Params.Item == nil || c.Params.Item.Seq != i {
			t.Errorf("packet %d: %+v", i+1, c.Params)
		}
	}
}

func TestUploadAnswersRepeatedRequests(t *testing.T) {
	h := newHarness(t, fast)
	asked := map[int]int{}
	h.onSend = func(c command.Command) {
		if c.Params.Item != nil && c.Params.Item.Seq == 2 {
			asked[2]++
			if asked[2] == 1 { // the item was lost: the drone asks again
				h.s.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: command.Sent})
				h.s.reply(command.Reply{DroneID: c.DroneID, Mission: &command.MissionEvent{Seq: 2}})
				return
			}
		}
		h.playDrone(c)
	}
	h.s.submitMission(context.Background(), missionReq())
	if u := h.final(); u.Status != command.Accepted {
		t.Fatalf("%+v", u)
	}
	if asked[2] != 2 {
		t.Errorf("item 2 sent %d times", asked[2])
	}
}

func TestUploadResendsAfterALostPacket(t *testing.T) {
	h := newHarness(t, fast)
	count := 0
	h.onSend = func(c command.Command) {
		if c.Params.Count > 0 {
			count++
			if count == 1 { // lost: sent, but the drone says nothing
				h.s.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: command.Sent})
				return
			}
		}
		h.playDrone(c)
	}
	h.s.submitMission(context.Background(), missionReq())
	if u := h.final(); u.Status != command.Accepted || count != 2 {
		t.Fatalf("%+v after %d counts", u, count)
	}
}

func TestUploadOutcomes(t *testing.T) {
	t.Run("the drone refuses", func(t *testing.T) {
		h := newHarness(t, fast)
		h.onSend = func(c command.Command) {
			h.s.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: command.Sent})
			h.s.reply(command.Reply{DroneID: c.DroneID, Mission: &command.MissionEvent{Ack: true, Result: uint8(common.MAV_MISSION_NO_SPACE)}})
		}
		h.s.submitMission(context.Background(), missionReq())
		if u := h.final(); u.Status != command.Rejected || u.Reason != "the mission is too long for the drone" {
			t.Errorf("%+v", u)
		}
	})
	t.Run("the drone goes quiet", func(t *testing.T) {
		h := newHarness(t, fast)
		h.onSend = func(c command.Command) {
			h.s.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: command.Sent})
		}
		h.s.submitMission(context.Background(), missionReq())
		if u := h.final(); u.Status != command.TimedOut || len(h.sends()) != fast.attempts {
			t.Errorf("%+v after %d packets", u, len(h.sends()))
		}
	})
	t.Run("the ingester is down", func(t *testing.T) {
		h := newHarness(t, fast)
		h.s.submitMission(context.Background(), missionReq())
		if u := h.final(); u.Status != command.Unreachable {
			t.Errorf("%+v", u)
		}
	})
}

func TestOneUploadAtATimeAndNoCommandsDuringIt(t *testing.T) {
	h := newHarness(t, fast)
	h.onSend = func(c command.Command) {
		h.s.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: command.Sent})
	}
	ctx := context.Background()
	h.s.submitMission(ctx, missionReq())
	if u := h.s.submitMission(ctx, missionReq()); u.Status != command.Rejected || u.Reason != "a mission upload is in progress" {
		t.Errorf("second upload: %+v", u)
	}
	if u := h.s.submit(ctx, armReq()); u.Status != command.Rejected || u.Reason != "a mission upload is in progress" {
		t.Errorf("command during upload: %+v", u)
	}
	other := armReq()
	other.DroneID = 43
	if u := h.s.submit(ctx, other); u.Status != command.Pending {
		t.Errorf("another drone: %+v", u)
	}
	h.final()
}

func TestBadMissionsNeverSent(t *testing.T) {
	h := newHarness(t, fast)
	req := missionReq()
	req.Waypoints = nil
	if u := h.s.submitMission(context.Background(), req); u.Status != command.Rejected {
		t.Errorf("%+v", u)
	}
	if len(h.sends()) != 0 {
		t.Error("it was sent")
	}
}
