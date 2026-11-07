package command

import (
	"errors"
	"fmt"
	"math"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
	"github.com/bluenviron/gomavlib/v3/pkg/message"
)

const (
	// MissionSubject is where the gateway sends a MissionRequest, by NATS
	// request and reply. The answer is the first Update.
	MissionSubject = "command_mission"
	// MissionGetSubject answers a MissionGet with a MissionView.
	MissionGetSubject = "command_mission_get"

	// Upload is the Update.Type of a mission upload.
	Upload = "mission_upload"
	// missionCount and missionItem are the upload's packets on command.<id>.
	// They are not types a browser can ask for: Validate refuses them.
	missionCount = "mission_count"
	missionItem  = "mission_item"

	// MaxWaypoints is the most waypoints a mission can have.
	MaxWaypoints = 100
)

// Waypoint is one point to fly over.
type Waypoint struct {
	Lat float64 `json:"lat"`
	Lon float64 `json:"lon"`
}

// MissionRequest is a mission for a drone: take off to AltitudeM, fly over the
// waypoints at that height, and return to launch.
type MissionRequest struct {
	DroneID   int64      `json:"drone_id"`
	Waypoints []Waypoint `json:"waypoints"`
	AltitudeM float64    `json:"altitude_m"`
	IssuedBy  string     `json:"issued_by"`
}

// MissionGet asks for the mission a drone holds.
type MissionGet struct {
	DroneID int64 `json:"drone_id"`
}

// MissionView is a drone's newest mission.
type MissionView struct {
	Waypoints []Waypoint `json:"waypoints"`
	AltitudeM float64    `json:"altitude_m"`
	Status    string     `json:"status"`
}

// MissionEvent is a drone's side of the upload: asking for item Seq, or, with
// Ack set, ending the upload with Result (MAV_MISSION_RESULT).
type MissionEvent struct {
	Seq    int   `json:"seq"`
	Ack    bool  `json:"ack,omitempty"`
	Result uint8 `json:"result,omitempty"`
}

// Item is one mission item as sent to the drone.
type Item struct {
	Seq     int     `json:"seq"`
	Command uint16  `json:"command"`
	Frame   uint8   `json:"frame"`
	Lat     float64 `json:"lat"`
	Lon     float64 `json:"lon"`
	Alt     float64 `json:"alt"`
}

// Validate reports whether a mission makes sense.
func (r MissionRequest) Validate() error {
	switch {
	case len(r.Waypoints) == 0:
		return errors.New("a mission needs at least one waypoint")
	case len(r.Waypoints) > MaxWaypoints:
		return fmt.Errorf("a mission can have %d waypoints at most", MaxWaypoints)
	case r.AltitudeM <= 0:
		return errors.New("a mission needs an altitude above 0")
	}
	for i, w := range r.Waypoints {
		if math.Abs(w.Lat) > 90 || math.Abs(w.Lon) > 180 || (w.Lat == 0 && w.Lon == 0) {
			return fmt.Errorf("waypoint %d is not a position", i+1)
		}
	}
	return nil
}

// Items turns a mission into what ArduPilot expects: item 0 is home, which it
// fills in itself, then takeoff, the waypoints, and return to launch.
func (r MissionRequest) Items() []Item {
	const (
		global   = uint8(common.MAV_FRAME_GLOBAL)
		relative = uint8(common.MAV_FRAME_GLOBAL_RELATIVE_ALT)
		waypoint = uint16(common.MAV_CMD_NAV_WAYPOINT)
	)
	first := r.Waypoints[0]
	items := []Item{
		{Seq: 0, Command: waypoint, Frame: global, Lat: first.Lat, Lon: first.Lon},
		{Seq: 1, Command: uint16(common.MAV_CMD_NAV_TAKEOFF), Frame: relative, Alt: r.AltitudeM},
	}
	for _, w := range r.Waypoints {
		items = append(items, Item{Seq: len(items), Command: waypoint, Frame: relative, Lat: w.Lat, Lon: w.Lon, Alt: r.AltitudeM})
	}
	return append(items, Item{Seq: len(items), Command: uint16(common.MAV_CMD_NAV_RETURN_TO_LAUNCH), Frame: relative})
}

// CountCommand and ItemCommand fill in c as one packet of an upload.
func CountCommand(c Command, count int) Command {
	c.Type, c.Params = missionCount, Params{Count: count}
	return c
}

func ItemCommand(c Command, it Item) Command {
	c.Type, c.Params = missionItem, Params{Item: &it}
	return c
}

// Message encodes a command for the drone.
func (c Command) Message(sysID, compID uint8) (message.Message, error) {
	switch c.Type {
	case missionCount:
		return &ardupilotmega.MessageMissionCount{
			TargetSystem: sysID, TargetComponent: compID, Count: uint16(c.Params.Count),
			MissionType: ardupilotmega.MAV_MISSION_TYPE_MISSION,
		}, nil
	case missionItem:
		it := c.Params.Item
		if it == nil {
			return nil, errors.New("mission item with no item")
		}
		return &ardupilotmega.MessageMissionItemInt{
			TargetSystem: sysID, TargetComponent: compID, Seq: uint16(it.Seq),
			Frame: common.MAV_FRAME(it.Frame), Command: common.MAV_CMD(it.Command),
			Autocontinue: 1, X: int32(math.Round(it.Lat * 1e7)), Y: int32(math.Round(it.Lon * 1e7)), Z: float32(it.Alt),
			MissionType: ardupilotmega.MAV_MISSION_TYPE_MISSION,
		}, nil
	}
	return c.Long(sysID, compID)
}

// MissionResultText says why a drone refused a mission.
func MissionResultText(r uint8) string {
	switch common.MAV_MISSION_RESULT(r) {
	case common.MAV_MISSION_NO_SPACE:
		return "the mission is too long for the drone"
	case common.MAV_MISSION_UNSUPPORTED_FRAME, common.MAV_MISSION_UNSUPPORTED:
		return "the drone does not support an item in this mission"
	case common.MAV_MISSION_INVALID, common.MAV_MISSION_INVALID_SEQUENCE:
		return "the drone reports the mission as invalid"
	case common.MAV_MISSION_DENIED:
		return "the drone is not accepting missions"
	}
	return fmt.Sprintf("the drone refused the mission (result %d)", r)
}
