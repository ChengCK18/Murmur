package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"sync"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/common"

	"github.com/ChengCK18/murmur/internal/command"
)

// timing holds the plan's limits, so tests can run them faster.
type timing struct {
	sent     time.Duration // no Sent reply by then: the ingester is down
	ack      time.Duration // no COMMAND_ACK by then: resend
	progress time.Duration // how long IN_PROGRESS holds a command open
	attempts int
	lifetime time.Duration
}

var planTiming = timing{
	sent:     time.Second,
	ack:      1500 * time.Millisecond,
	progress: 5 * time.Second,
	attempts: 3,
	lifetime: command.Lifetime,
}

// store is the audit log. A command is written before it is sent, so nothing
// reaches a drone without a record.
type store interface {
	insert(ctx context.Context, c command.Command, status, reason string) error
	update(ctx context.Context, u command.Update) error
}

// key is what a COMMAND_ACK can be matched on: it names a command type and no
// request ID, hence one command in flight per drone per type (plan §4).
type key struct {
	drone int64
	cmd   common.MAV_CMD
}

// flight is one command being tracked.
type flight struct {
	c       command.Command
	replies chan command.Reply
}

type service struct {
	publish func(subject string, data []byte) error
	store   store
	check   func(context.Context, command.Request) string // why a drone cannot take a command; "": it can
	timing  timing
	now     func() time.Time
	obs     func(status string) // every final status; nil: none

	mu     sync.Mutex
	flying map[key]*flight
	byID   map[string]*flight
	wg     sync.WaitGroup
}

func newService(publish func(string, []byte) error, s store, t timing) *service {
	return &service{publish: publish, store: s, timing: t, now: time.Now,
		check:  func(context.Context, command.Request) string { return "" },
		flying: map[key]*flight{}, byID: map[string]*flight{}}
}

// submit starts a command and returns its first status. A command it refuses
// comes back rejected, with the reason, and never reaches the drone.
func (s *service) submit(ctx context.Context, req command.Request) command.Update {
	now := s.now()
	c := command.Command{DroneID: req.DroneID, Type: req.Type, Params: req.Params, IssuedBy: req.IssuedBy,
		IssuedAt: now, ExpiresAt: now.Add(s.timing.lifetime)}
	refuse := func(reason string) command.Update {
		s.count(command.Rejected)
		return command.Update{DroneID: c.DroneID, Type: c.Type, Status: command.Rejected, Reason: reason, IssuedBy: c.IssuedBy, At: now}
	}
	if req.IssuedBy == "" {
		return refuse("no user")
	}
	if err := command.Validate(req.Type, req.Params); err != nil {
		return refuse(err.Error())
	}
	id, err := newID()
	if err != nil {
		return refuse("could not start the command")
	}
	c.CommandID = id
	if reason := s.check(ctx, req); reason != "" {
		return s.refused(ctx, c, reason)
	}

	mav, _ := command.MavCommand(c.Type)
	k := key{c.DroneID, mav}
	f := &flight{c: c, replies: make(chan command.Reply, 16)}
	if !s.board(k, f) {
		return s.refused(ctx, c, "already in progress")
	}
	// Written first: no record, no command.
	if err := s.store.insert(ctx, c, command.Pending, ""); err != nil {
		s.land(k, f)
		log.Printf("command %s not recorded: %v", c.CommandID, err)
		return refuse("the audit log is not available")
	}
	s.wg.Go(func() {
		defer s.land(k, f)
		u := s.fly(ctx, f)
		s.report(u)
		s.count(u.Status)
	})
	return s.update(c, command.Pending, "", 0, nil)
}

// refused records a command turned away, so the log has it too.
func (s *service) refused(ctx context.Context, c command.Command, reason string) command.Update {
	if err := s.store.insert(ctx, c, command.Rejected, reason); err != nil {
		log.Printf("command %s not recorded: %v", c.CommandID, err)
	}
	s.count(command.Rejected)
	u := s.update(c, command.Rejected, reason, 0, nil)
	s.tell(u) // other browsers watching this drone see it too
	return u
}

func (s *service) count(status string) {
	if s.obs != nil {
		s.obs(status)
	}
}

func (s *service) board(k key, f *flight) bool {
	s.mu.Lock()
	defer s.mu.Unlock()
	if _, busy := s.flying[k]; busy {
		return false
	}
	s.flying[k] = f
	s.byID[f.c.CommandID] = f
	return true
}

func (s *service) land(k key, f *flight) {
	s.mu.Lock()
	defer s.mu.Unlock()
	delete(s.flying, k)
	delete(s.byID, f.c.CommandID)
}

// reply passes a message from command_reply.<drone_id> to its command. One for
// a command no longer tracked is dropped: that attempt is already decided.
func (s *service) reply(r command.Reply) {
	s.mu.Lock()
	var f *flight
	if r.Ack != nil {
		f = s.flying[key{r.DroneID, common.MAV_CMD(r.Ack.Command)}]
	} else {
		f = s.byID[r.CommandID]
	}
	s.mu.Unlock()
	if f == nil {
		return
	}
	select {
	case f.replies <- r:
	default: // a drone repeating itself; the buffer holds all that matters
	}
}

// fly runs a command to a final status.
func (s *service) fly(ctx context.Context, f *flight) command.Update {
	c := f.c
	deadline := time.NewTimer(time.Until(c.ExpiresAt))
	defer deadline.Stop()
	reported := false // Sent goes out once, however many attempts it takes

	for attempt := 1; attempt <= s.timing.attempts; attempt++ {
		c.Attempt = attempt
		if err := s.send(c); err != nil {
			return s.update(c, command.Unreachable, "could not reach the ingester", attempt, nil)
		}
		sentBy := time.NewTimer(s.timing.sent)
		ackBy := time.NewTimer(s.timing.ack)
		u, again := s.await(ctx, c, f, deadline, sentBy, ackBy, &reported)
		sentBy.Stop()
		ackBy.Stop()
		if !again {
			return u
		}
	}
	return s.update(c, command.TimedOut, fmt.Sprintf("no answer after %d attempts", s.timing.attempts), s.timing.attempts, nil)
}

// await waits for the outcome of one attempt. It returns again = true to
// resend.
func (s *service) await(ctx context.Context, c command.Command, f *flight, deadline, sentBy, ackBy *time.Timer, reported *bool) (command.Update, bool) {
	sent := sentBy.C
	for {
		select {
		case <-ctx.Done():
			// Left as it stands: the next start marks it unknown.
			return s.update(c, command.Unknown, "the command service stopped", c.Attempt, nil), false

		case r := <-f.replies:
			switch {
			case r.Ack != nil:
				if ardupilotmega.MAV_RESULT(r.Ack.Result) == ardupilotmega.MAV_RESULT_IN_PROGRESS {
					// The drone has it, so it no longer expires or resends.
					ackBy.Reset(s.timing.progress)
					deadline.Stop()
					sent = nil
					continue
				}
				status, reason := outcome(ardupilotmega.MAV_RESULT(r.Ack.Result))
				result := r.Ack.Result
				return s.update(c, status, reason, c.Attempt, &result), false
			case r.Status == command.Sent:
				if r.Attempt == c.Attempt {
					sent = nil
				}
				if !*reported {
					*reported = true
					s.report(s.update(c, command.Sent, "", c.Attempt, nil))
				}
			case r.Status == command.Expired:
				return s.update(c, command.Expired, "past its expiry at the ingester", c.Attempt, nil), false
			case r.Status == command.Unreachable:
				return s.update(c, command.Unreachable, "the ingester has not heard this drone since it started", c.Attempt, nil), false
			}

		case <-sent:
			return s.update(c, command.Unreachable, "no reply from the ingester", c.Attempt, nil), false

		case <-ackBy.C:
			return command.Update{}, true

		case <-deadline.C:
			return s.update(c, command.TimedOut, "no answer before it expired", c.Attempt, nil), false
		}
	}
}

// outcome maps a final COMMAND_ACK.
func outcome(r ardupilotmega.MAV_RESULT) (status, reason string) {
	switch r {
	case ardupilotmega.MAV_RESULT_ACCEPTED:
		return command.Accepted, ""
	case ardupilotmega.MAV_RESULT_FAILED:
		return command.Failed, "the drone reported failure"
	case ardupilotmega.MAV_RESULT_DENIED:
		return command.Rejected, "denied by the drone"
	case ardupilotmega.MAV_RESULT_UNSUPPORTED:
		return command.Rejected, "not supported by the drone"
	case ardupilotmega.MAV_RESULT_TEMPORARILY_REJECTED:
		return command.Rejected, "temporarily rejected by the drone"
	}
	return command.Rejected, fmt.Sprintf("the drone answered with result %d", r)
}

func (s *service) send(c command.Command) error {
	data, err := json.Marshal(c)
	if err != nil {
		return err
	}
	return s.publish(command.Subject(c.DroneID), data)
}

func (s *service) update(c command.Command, status, reason string, attempts int, result *uint8) command.Update {
	return command.Update{CommandID: c.CommandID, DroneID: c.DroneID, Type: c.Type, Status: status, Reason: reason,
		Attempts: attempts, MavResult: result, IssuedBy: c.IssuedBy, At: s.now()}
}

// report records a status and tells the browsers.
func (s *service) report(u command.Update) {
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := s.store.update(ctx, u); err != nil {
		log.Printf("command %s: recording %s: %v", u.CommandID, u.Status, err)
	}
	s.tell(u)
}

func (s *service) tell(u command.Update) {
	data, err := json.Marshal(u)
	if err == nil {
		err = s.publish(command.StatusSubject(u.DroneID), data)
	}
	if err != nil {
		log.Printf("command %s: publishing %s: %v", u.CommandID, u.Status, err)
	}
}

// newID returns a random (version 4) UUID.
func newID() (string, error) {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "", err
	}
	b[6] = b[6]&0x0f | 0x40
	b[8] = b[8]&0x3f | 0x80
	h := hex.EncodeToString(b[:])
	return h[:8] + "-" + h[8:12] + "-" + h[12:16] + "-" + h[16:20] + "-" + h[20:], nil
}
