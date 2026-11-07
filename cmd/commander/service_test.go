package main

import (
	"context"
	"encoding/json"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"

	"github.com/ChengCK18/murmur/internal/command"
)

var fast = timing{sent: 40 * time.Millisecond, ack: 60 * time.Millisecond, progress: 200 * time.Millisecond, attempts: 3, lifetime: 500 * time.Millisecond}

// harness plays the ingester and the drone.
type harness struct {
	t        *testing.T
	s        *service
	mu       sync.Mutex
	sent     []command.Command
	statuses []command.Update
	rows     map[string]string
	onSend   func(command.Command)
	failDB   bool
}

func newHarness(t *testing.T, tm timing) *harness {
	h := &harness{t: t, rows: map[string]string{}}
	h.s = newService(h.publish, h, tm)
	return h
}

func (h *harness) publish(subject string, data []byte) error {
	if strings.HasPrefix(subject, "command_status.") {
		var u command.Update
		if err := json.Unmarshal(data, &u); err != nil {
			h.t.Error(err)
		}
		h.mu.Lock()
		h.statuses = append(h.statuses, u)
		h.mu.Unlock()
		return nil
	}
	var c command.Command
	if err := json.Unmarshal(data, &c); err != nil {
		h.t.Error(err)
	}
	h.mu.Lock()
	h.sent = append(h.sent, c)
	on := h.onSend
	h.mu.Unlock()
	if on != nil {
		go on(c)
	}
	return nil
}

func (h *harness) insert(_ context.Context, c command.Command, status, _ string) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.failDB {
		return context.DeadlineExceeded
	}
	h.rows[c.CommandID] = status
	return nil
}

func (h *harness) update(_ context.Context, u command.Update) error {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.rows[u.CommandID] = u.Status
	return nil
}

func (h *harness) final() command.Update {
	h.t.Helper()
	done := make(chan struct{})
	go func() { h.s.wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		h.t.Fatal("the command never finished")
	}
	h.mu.Lock()
	defer h.mu.Unlock()
	if len(h.statuses) == 0 {
		h.t.Fatal("no status published")
	}
	return h.statuses[len(h.statuses)-1]
}

func (h *harness) sends() []command.Command {
	h.mu.Lock()
	defer h.mu.Unlock()
	return append([]command.Command(nil), h.sent...)
}

func sentReply(c command.Command) command.Reply {
	return command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: command.Sent}
}

func ack(c command.Command, r ardupilotmega.MAV_RESULT) command.Reply {
	cmd, _ := command.MavCommand(c.Type)
	return command.Reply{DroneID: c.DroneID, Ack: &command.Ack{Command: uint16(cmd), Result: uint8(r)}}
}

func armReq() command.Request {
	return command.Request{DroneID: 42, Type: command.Arm, IssuedBy: "demo"}
}

func TestResults(t *testing.T) {
	cases := []struct {
		result ardupilotmega.MAV_RESULT
		status string
	}{
		{ardupilotmega.MAV_RESULT_ACCEPTED, command.Accepted},
		{ardupilotmega.MAV_RESULT_DENIED, command.Rejected},
		{ardupilotmega.MAV_RESULT_UNSUPPORTED, command.Rejected},
		{ardupilotmega.MAV_RESULT_TEMPORARILY_REJECTED, command.Rejected},
		{ardupilotmega.MAV_RESULT_FAILED, command.Failed},
	}
	for _, c := range cases {
		t.Run(c.status, func(t *testing.T) {
			h := newHarness(t, fast)
			h.onSend = func(cmd command.Command) {
				h.s.reply(sentReply(cmd))
				h.s.reply(ack(cmd, c.result))
			}
			first := h.s.submit(context.Background(), armReq())
			u := h.final()
			if first.Status != command.Pending || u.Status != c.status || u.Attempts != 1 {
				t.Errorf("first %s, final %+v", first.Status, u)
			}
			if u.MavResult == nil || *u.MavResult != uint8(c.result) {
				t.Errorf("result %v", u.MavResult)
			}
			if h.rows[u.CommandID] != c.status {
				t.Errorf("recorded %q", h.rows[u.CommandID])
			}
		})
	}
}

func TestResendsWithoutAnAck(t *testing.T) {
	h := newHarness(t, fast)
	var mu sync.Mutex
	h.onSend = func(c command.Command) {
		h.s.reply(sentReply(c))
		mu.Lock()
		defer mu.Unlock()
		if c.Attempt == 3 { // the first two packets were lost
			h.s.reply(ack(c, ardupilotmega.MAV_RESULT_ACCEPTED))
		}
	}
	h.s.submit(context.Background(), armReq())
	u := h.final()
	sends := h.sends()
	if u.Status != command.Accepted || u.Attempts != 3 || len(sends) != 3 {
		t.Fatalf("final %+v after %d sends", u, len(sends))
	}
	for i, c := range sends {
		if c.Attempt != i+1 || c.ExpiresAt != sends[0].ExpiresAt {
			t.Errorf("send %d: attempt %d, expires %v", i, c.Attempt, c.ExpiresAt)
		}
	}
}

func TestTimesOutAfterThreeAttempts(t *testing.T) {
	h := newHarness(t, fast)
	h.onSend = func(c command.Command) { h.s.reply(sentReply(c)) }
	h.s.submit(context.Background(), armReq())
	u := h.final()
	if u.Status != command.TimedOut || len(h.sends()) != 3 {
		t.Errorf("final %+v after %d sends", u, len(h.sends()))
	}
}

func TestNoAttemptAfterExpiry(t *testing.T) {
	tm := fast
	tm.lifetime = 100 * time.Millisecond // room for two attempts of three
	h := newHarness(t, tm)
	h.onSend = func(c command.Command) { h.s.reply(sentReply(c)) }
	h.s.submit(context.Background(), armReq())
	u := h.final()
	if u.Status != command.TimedOut || len(h.sends()) != 2 {
		t.Errorf("final %+v after %d sends", u, len(h.sends()))
	}
}

func TestIngesterDownIsUnreachableAtOnce(t *testing.T) {
	h := newHarness(t, fast) // no Sent reply ever comes
	start := time.Now()
	h.s.submit(context.Background(), armReq())
	u := h.final()
	if u.Status != command.Unreachable || len(h.sends()) != 1 {
		t.Errorf("final %+v after %d sends", u, len(h.sends()))
	}
	if took := time.Since(start); took < fast.sent || took > fast.ack {
		t.Errorf("took %v, want about %v", took, fast.sent)
	}
}

func TestIngesterRefusals(t *testing.T) {
	for _, status := range []string{command.Expired, command.Unreachable} {
		t.Run(status, func(t *testing.T) {
			h := newHarness(t, fast)
			h.onSend = func(c command.Command) {
				h.s.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: status})
			}
			h.s.submit(context.Background(), armReq())
			if u := h.final(); u.Status != status || len(h.sends()) != 1 {
				t.Errorf("final %+v after %d sends", u, len(h.sends()))
			}
		})
	}
}

func TestInProgressKeepsWaitingWithoutResending(t *testing.T) {
	h := newHarness(t, fast)
	h.onSend = func(c command.Command) {
		h.s.reply(sentReply(c))
		h.s.reply(ack(c, ardupilotmega.MAV_RESULT_IN_PROGRESS))
		time.Sleep(fast.lifetime + 50*time.Millisecond) // past expiry and every ack timeout
		h.s.reply(ack(c, ardupilotmega.MAV_RESULT_ACCEPTED))
	}
	tm := fast
	tm.progress = time.Second
	h.s.timing = tm
	h.s.submit(context.Background(), armReq())
	if u := h.final(); u.Status != command.Accepted || len(h.sends()) != 1 {
		t.Errorf("final %+v after %d sends", u, len(h.sends()))
	}
}

func TestOneInFlightPerDronePerType(t *testing.T) {
	h := newHarness(t, fast)
	h.onSend = func(c command.Command) { h.s.reply(sentReply(c)) }
	ctx := context.Background()
	h.s.submit(ctx, armReq())

	second := armReq()
	second.Type = command.Disarm // the same MAVLink command, so its ack could not be told apart
	if u := h.s.submit(ctx, second); u.Status != command.Rejected || u.Reason != "already in progress" {
		t.Errorf("same type: %+v", u)
	}
	if u := h.s.submit(ctx, command.Request{DroneID: 42, Type: command.Land, IssuedBy: "demo"}); u.Status != command.Pending {
		t.Errorf("another type: %+v", u)
	}
	if u := h.s.submit(ctx, command.Request{DroneID: 43, Type: command.Arm, IssuedBy: "demo"}); u.Status != command.Pending {
		t.Errorf("another drone: %+v", u)
	}
	h.final()
	// Finished, so the same command can go again.
	if u := h.s.submit(ctx, armReq()); u.Status != command.Pending {
		t.Errorf("after finishing: %+v", u)
	}
	h.final()
}

func TestAckGoesToItsCommand(t *testing.T) {
	h := newHarness(t, fast)
	h.onSend = func(c command.Command) {
		h.s.reply(sentReply(c))
		if c.Type == command.Land {
			h.s.reply(ack(c, ardupilotmega.MAV_RESULT_ACCEPTED))
		}
	}
	ctx := context.Background()
	h.s.submit(ctx, armReq())
	h.s.submit(ctx, command.Request{DroneID: 42, Type: command.Land, IssuedBy: "demo"})
	h.final()
	got := map[string]string{}
	for _, u := range h.statuses {
		if command.Final(u.Status) {
			got[u.Type] = u.Status
		}
	}
	if got[command.Land] != command.Accepted || got[command.Arm] != command.TimedOut {
		t.Errorf("finals %v", got)
	}
}

func TestRefusedBeforeSending(t *testing.T) {
	cases := map[string]command.Request{
		"no user":      {DroneID: 42, Type: command.Arm},
		"unknown type": {DroneID: 42, Type: "self_destruct", IssuedBy: "demo"},
		"bad params":   {DroneID: 42, Type: command.Takeoff, IssuedBy: "demo"},
	}
	for name, req := range cases {
		t.Run(name, func(t *testing.T) {
			h := newHarness(t, fast)
			if u := h.s.submit(context.Background(), req); u.Status != command.Rejected {
				t.Errorf("%+v", u)
			}
			if len(h.sends()) != 0 {
				t.Error("it was sent")
			}
		})
	}
}

func TestNoAuditLogNoCommand(t *testing.T) {
	h := newHarness(t, fast)
	h.failDB = true
	u := h.s.submit(context.Background(), armReq())
	if u.Status != command.Rejected || len(h.sends()) != 0 {
		t.Errorf("%+v after %d sends", u, len(h.sends()))
	}
	h.failDB = false // and the failure does not leave the type stuck in flight
	h.onSend = func(c command.Command) { h.s.reply(sentReply(c)); h.s.reply(ack(c, ardupilotmega.MAV_RESULT_ACCEPTED)) }
	if u := h.s.submit(context.Background(), armReq()); u.Status != command.Pending {
		t.Errorf("after recovery: %+v", u)
	}
	h.final()
}

func TestSentReportedOnce(t *testing.T) {
	h := newHarness(t, fast)
	h.onSend = func(c command.Command) {
		h.s.reply(sentReply(c))
		if c.Attempt == 2 {
			h.s.reply(ack(c, ardupilotmega.MAV_RESULT_ACCEPTED))
		}
	}
	h.s.submit(context.Background(), armReq())
	h.final()
	n := 0
	for _, u := range h.statuses {
		if u.Status == command.Sent {
			n++
		}
	}
	if n != 1 {
		t.Errorf("sent reported %d times", n)
	}
}

func TestUUID(t *testing.T) {
	id, err := newID()
	if err != nil {
		t.Fatal(err)
	}
	if len(id) != 36 || id[14] != '4' || id[8] != '-' {
		t.Errorf("id %q", id)
	}
}
