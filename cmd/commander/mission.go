package main

import (
	"context"
	"fmt"
	"log"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"

	"github.com/ChengCK18/murmur/internal/command"
)

// missionStore records missions. The drone holds the real thing; this is what
// the map shows and what Start mission checks.
type missionStore interface {
	insertMission(context.Context, command.Command, command.MissionRequest) error
	finishMission(ctx context.Context, id string, uploaded bool) error
}

// upload is one mission going to a drone (plan §4). The drone leads: after
// MISSION_COUNT it asks for each item, perhaps more than once, and ends with
// MISSION_ACK. Each request is answered even if it is a repeat.
type upload struct {
	c       command.Command
	items   []command.Item
	replies chan command.Reply
}

func (s *service) uploading(drone int64) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.uploads[drone] != nil
}

// submitMission starts an upload and returns its first status. Only one runs
// per drone, and other commands to that drone are refused until it ends: a
// command would otherwise land in the middle of the handshake.
func (s *service) submitMission(ctx context.Context, req command.MissionRequest) command.Update {
	now := s.now()
	c := command.Command{DroneID: req.DroneID, Type: command.Upload, IssuedBy: req.IssuedBy, IssuedAt: now, ExpiresAt: now.Add(s.timing.lifetime)}
	refuse := func(reason string) command.Update {
		s.count(command.Rejected)
		return command.Update{DroneID: c.DroneID, Type: c.Type, Status: command.Rejected, Reason: reason, IssuedBy: c.IssuedBy, At: now}
	}
	if req.IssuedBy == "" {
		return refuse("no user")
	}
	if err := req.Validate(); err != nil {
		return refuse(err.Error())
	}
	id, err := newID()
	if err != nil {
		return refuse("could not start the upload")
	}
	c.CommandID = id

	u := &upload{c: c, items: req.Items(), replies: make(chan command.Reply, 64)}
	s.mu.Lock()
	busy := s.uploads[c.DroneID] != nil
	if !busy {
		s.uploads[c.DroneID] = u
	}
	s.mu.Unlock()
	if busy {
		return s.refused(ctx, c, "a mission upload is in progress")
	}
	done := func() {
		s.mu.Lock()
		delete(s.uploads, c.DroneID)
		s.mu.Unlock()
	}
	if err := s.store.insert(ctx, c, command.Pending, ""); err != nil {
		done()
		log.Printf("command %s not recorded: %v", c.CommandID, err)
		return refuse("the audit log is not available")
	}
	if s.missions != nil {
		if err := s.missions.insertMission(ctx, c, req); err != nil {
			done()
			log.Printf("mission %s not recorded: %v", c.CommandID, err)
			s.report(s.update(c, command.Failed, "could not record the mission", 0, nil))
			return refuse("the mission could not be recorded")
		}
	}
	s.wg.Go(func() {
		defer done()
		final := s.runUpload(ctx, u)
		if s.missions != nil {
			wctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			if err := s.missions.finishMission(wctx, c.CommandID, final.Status == command.Accepted); err != nil {
				log.Print(err)
			}
		}
		s.report(final)
		s.count(final.Status)
	})
	return s.update(c, command.Pending, "", 0, nil)
}

// runUpload drives the handshake to a final status. Each packet gets its own
// short expiry, so a stalled upload can never deliver a late item, and each
// step has the same timeout and retries as a command.
func (s *service) runUpload(ctx context.Context, u *upload) command.Update {
	c := u.c
	packets := 0
	send := func(p command.Command) (<-chan time.Time, error) {
		packets++
		p.Attempt = packets
		p.ExpiresAt = s.now().Add(s.timing.lifetime)
		if err := s.send(p); err != nil {
			return nil, err
		}
		return time.After(s.timing.sent), nil
	}

	last := command.CountCommand(c, len(u.items))
	sent, err := send(last)
	if err != nil {
		return s.update(c, command.Unreachable, "could not reach the ingester", packets, nil)
	}
	step := time.NewTimer(s.timing.ack)
	defer step.Stop()
	tries := 1
	reported := false

	for {
		select {
		case <-ctx.Done():
			return s.update(c, command.Unknown, "the command service stopped", packets, nil)

		case r := <-u.replies:
			switch {
			case r.Mission != nil && r.Mission.Ack:
				if common.MAV_MISSION_RESULT(r.Mission.Result) == common.MAV_MISSION_ACCEPTED {
					return s.update(c, command.Accepted, "", packets, nil)
				}
				result := r.Mission.Result
				return s.update(c, command.Rejected, command.MissionResultText(result), packets, &result)

			case r.Mission != nil:
				if r.Mission.Seq < 0 || r.Mission.Seq >= len(u.items) {
					continue // not an item of this mission: the timeout covers it
				}
				last = command.ItemCommand(c, u.items[r.Mission.Seq])
				if sent, err = send(last); err != nil {
					return s.update(c, command.Unreachable, "could not reach the ingester", packets, nil)
				}
				tries = 1
				step.Reset(s.timing.ack)

			case r.Status == command.Sent:
				if r.Attempt == packets {
					sent = nil
				}
				if !reported {
					reported = true
					s.report(s.update(c, command.Sent, "", packets, nil))
				}
			case r.Status == command.Expired:
				return s.update(c, command.Expired, "past its expiry at the ingester", packets, nil)
			case r.Status == command.Unreachable:
				return s.update(c, command.Unreachable, "the ingester has not heard this drone since it started", packets, nil)
			}

		case <-sent:
			return s.update(c, command.Unreachable, "no reply from the ingester", packets, nil)

		case <-step.C:
			if tries >= s.timing.attempts {
				return s.update(c, command.TimedOut, fmt.Sprintf("no answer from the drone after %d tries", tries), packets, nil)
			}
			tries++
			if sent, err = send(last); err != nil {
				return s.update(c, command.Unreachable, "could not reach the ingester", packets, nil)
			}
			step.Reset(s.timing.ack)
		}
	}
}
