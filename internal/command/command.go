// Package command defines the messages of the command path (plan §4) and how
// a command becomes MAVLink.
//
// Commands are built the opposite way round from telemetry: one that cannot be
// delivered now must fail visibly, never run late. They travel on core NATS,
// outside any stream, and expire after Lifetime.
//
//	gateway → command_request → commander
//	commander → command.<drone_id> → ingester → COMMAND_LONG → drone
//	drone → COMMAND_ACK → ingester → command_reply.<drone_id> → commander
//	commander → command_status.<drone_id> → gateway
package command

import (
	"errors"
	"fmt"
	"strconv"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"
)

const (
	// RequestSubject is where the gateway sends a Request, by NATS request and
	// reply. The answer is the first Update.
	RequestSubject = "command_request"
	// FleetSubject is where the gateway sends a FleetRequest, by NATS request
	// and reply. The answer is a FleetStarted; each drone's statuses then follow
	// on its own command_status subject.
	FleetSubject = "command_fleet"
	// RecentSubject answers a RecentRequest with a drone's latest commands, so
	// a reconnected browser can show them again.
	RecentSubject = "command_recent"
	// Subjects is what the ingester subscribes to.
	Subjects = "command.*"
	// ReplySubjects is what the commander subscribes to.
	ReplySubjects = "command_reply.*"
	// StatusSubjects is what the gateway subscribes to.
	StatusSubjects = "command_status.*"

	// MaxFleet is the most drones one fleet command can go to.
	MaxFleet = 1000
	// Lifetime is how long a command may still be carried out.
	Lifetime = 5 * time.Second
	// SenderSystem and SenderComponent identify the platform to a drone as a
	// ground station, which is what ArduPilot expects.
	SenderSystem    = 255
	SenderComponent = 190 // MAV_COMP_ID_MISSIONPLANNER
)

// Subject is the subject a command to a drone is published on.
func Subject(droneID int64) string { return "command." + strconv.FormatInt(droneID, 10) }

// ReplySubject is the subject of replies about a drone's commands.
func ReplySubject(droneID int64) string {
	return "command_reply." + strconv.FormatInt(droneID, 10)
}

// StatusSubject is the subject of status updates for a drone's commands.
func StatusSubject(droneID int64) string {
	return "command_status." + strconv.FormatInt(droneID, 10)
}

// Command types.
const (
	Arm          = "arm"
	Disarm       = "disarm"
	SetMode      = "set_mode"
	Takeoff      = "takeoff"
	ReturnHome   = "return_to_launch"
	Land         = "land"
	MissionStart = "mission_start"
)

// Statuses. A command ends in one of the last seven; Unknown is what a
// restarted commander marks commands it was still tracking.
const (
	Pending     = "pending"
	Sent        = "sent"
	Accepted    = "accepted"
	Rejected    = "rejected"
	Failed      = "failed"
	TimedOut    = "timed_out"
	Expired     = "expired"
	Unreachable = "unreachable"
	Unknown     = "unknown"
)

// Params are a command's arguments. Each type uses only its own.
type Params struct {
	AltitudeM float64 `json:"altitude_m,omitempty"` // takeoff
	Mode      string  `json:"mode,omitempty"`       // set_mode
}

// Request is what the gateway asks for on behalf of a logged-in user.
type Request struct {
	DroneID  int64  `json:"drone_id"`
	Type     string `json:"type"`
	Params   Params `json:"params"`
	IssuedBy string `json:"issued_by"`
	FleetID  string `json:"fleet_id,omitempty"` // set by the commander
}

// FleetRequest is one command to several drones. Each drone's command is
// tracked on its own.
type FleetRequest struct {
	DroneIDs []int64 `json:"drone_ids"`
	Type     string  `json:"type"`
	Params   Params  `json:"params"`
	IssuedBy string  `json:"issued_by"`
}

// FleetStarted answers a FleetRequest. Count is the number of drones whose
// status will follow; if Error is set, none will.
type FleetStarted struct {
	FleetID string `json:"fleet_id,omitempty"`
	Count   int    `json:"count"`
	Error   string `json:"error,omitempty"`
}

// RecentRequest asks for a drone's latest commands, newest first, or for every
// command of a fleet command if FleetID is set.
type RecentRequest struct {
	DroneID int64  `json:"drone_id,omitempty"`
	FleetID string `json:"fleet_id,omitempty"`
	Limit   int    `json:"limit"`
}

// Command is the message on command.<drone_id>.
type Command struct {
	CommandID string    `json:"command_id"`
	FleetID   string    `json:"fleet_id,omitempty"`
	DroneID   int64     `json:"drone_id"`
	Type      string    `json:"type"`
	Params    Params    `json:"params"`
	Attempt   int       `json:"attempt"`
	IssuedBy  string    `json:"issued_by"`
	IssuedAt  time.Time `json:"issued_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

// Ack is a drone's COMMAND_ACK, which names a command type but no command ID.
type Ack struct {
	Command  uint16 `json:"command"`
	Result   uint8  `json:"result"` // MAV_RESULT
	Progress uint8  `json:"progress,omitempty"`
}

// Reply is the message on command_reply.<drone_id>: either the ingester
// reporting what it did with an attempt (Status Sent, Expired or Unreachable,
// with the CommandID), or a drone's Ack.
type Reply struct {
	DroneID   int64  `json:"drone_id"`
	CommandID string `json:"command_id,omitempty"`
	Attempt   int    `json:"attempt,omitempty"`
	Status    string `json:"status,omitempty"`
	Ack       *Ack   `json:"ack,omitempty"`
}

// Update is the message on command_status.<drone_id>.
type Update struct {
	CommandID string    `json:"command_id,omitempty"` // empty if refused before it had one
	FleetID   string    `json:"fleet_id,omitempty"`
	DroneID   int64     `json:"drone_id"`
	Type      string    `json:"type"`
	Status    string    `json:"status"`
	Reason    string    `json:"reason,omitempty"`
	Attempts  int       `json:"attempts"`
	MavResult *uint8    `json:"mav_result,omitempty"`
	IssuedBy  string    `json:"issued_by"`
	At        time.Time `json:"at"`
}

// Final reports whether a status ends a command.
func Final(status string) bool {
	switch status {
	case Accepted, Rejected, Failed, TimedOut, Expired, Unreachable, Unknown:
		return true
	}
	return false
}

// copterModes are the ArduPilot Copter mode numbers commands can select.
var copterModes = map[string]float32{"AUTO": 3, "GUIDED": 4, "RTL": 6, "LAND": 9}

// ErrUnknownType is returned for a command type not in the plan's table.
var ErrUnknownType = errors.New("unknown command type")

// MavCommand returns the MAVLink command a type is sent as, which is also what
// its COMMAND_ACK names.
func MavCommand(typ string) (common.MAV_CMD, error) {
	switch typ {
	case Arm, Disarm:
		return common.MAV_CMD_COMPONENT_ARM_DISARM, nil
	case SetMode:
		return common.MAV_CMD_DO_SET_MODE, nil
	case Takeoff:
		return common.MAV_CMD_NAV_TAKEOFF, nil
	case ReturnHome:
		return common.MAV_CMD_NAV_RETURN_TO_LAUNCH, nil
	case Land:
		return common.MAV_CMD_NAV_LAND, nil
	case MissionStart:
		return common.MAV_CMD_MISSION_START, nil
	}
	return 0, fmt.Errorf("%w %q", ErrUnknownType, typ)
}

// Validate reports whether a command type and its arguments make sense,
// before any drone state comes into it.
func Validate(typ string, p Params) error {
	if _, err := MavCommand(typ); err != nil {
		return err
	}
	switch typ {
	case SetMode:
		if _, ok := copterModes[p.Mode]; !ok {
			return fmt.Errorf("unknown mode %q", p.Mode)
		}
	case Takeoff:
		if p.AltitudeM <= 0 {
			return errors.New("takeoff needs an altitude above 0")
		}
	}
	return nil
}

// Long encodes a command as COMMAND_LONG. Each resend raises confirmation by
// one, so the drone knows it is a repeat.
func (c Command) Long(sysID, compID uint8) (*ardupilotmega.MessageCommandLong, error) {
	if err := Validate(c.Type, c.Params); err != nil {
		return nil, err
	}
	cmd, _ := MavCommand(c.Type)
	m := &ardupilotmega.MessageCommandLong{
		TargetSystem:    sysID,
		TargetComponent: compID,
		Command:         cmd,
		Confirmation:    uint8(max(c.Attempt-1, 0)),
	}
	switch c.Type {
	case Arm:
		m.Param1 = 1
	case SetMode:
		m.Param1 = float32(ardupilotmega.MAV_MODE_FLAG_CUSTOM_MODE_ENABLED)
		m.Param2 = copterModes[c.Params.Mode]
	case Takeoff:
		m.Param7 = float32(c.Params.AltitudeM)
	}
	return m, nil
}
