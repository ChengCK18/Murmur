package main

import (
	"encoding/json"
	"log"
	"net/netip"
	"sync"
	"sync/atomic"
	"time"

	"github.com/bluenviron/gomavlib/v3/pkg/dialect"
	"github.com/bluenviron/gomavlib/v3/pkg/dialects/ardupilotmega"
	"github.com/bluenviron/gomavlib/v3/pkg/frame"
	"github.com/bluenviron/gomavlib/v3/pkg/message"

	"github.com/ChengCK18/murmur/internal/command"
	"github.com/ChengCK18/murmur/internal/registry"
)

// relay carries commands to drones and their answers back (plan §4). It holds
// no command logic: it refuses what has expired or has nowhere to go, sends
// the rest, and reports which. The commander decides everything else.
//
// It sends from the socket the ingester reads on: drones, and the NAT routers
// in front of real ones, expect replies from the address they talk to.
type relay struct {
	in      *ingester
	send    func(b []byte, to netip.AddrPort) (int, error)
	publish func(subject string, data []byte) error
	now     func() time.Time
	rw      *dialect.ReadWriter

	mu      sync.Mutex
	writers map[registry.ID]*droneWriter

	sent, expired, unreachable atomic.Uint64
}

// droneWriter sends to one drone. Each has its own frame.Writer, and so its own
// MAVLink sequence counter.
type droneWriter struct {
	r  *relay
	to netip.AddrPort
	w  *frame.Writer
}

func (d *droneWriter) Write(p []byte) (int, error) { return d.r.send(p, d.to) }

func newRelay(in *ingester, send func([]byte, netip.AddrPort) (int, error), publish func(string, []byte) error) (*relay, error) {
	rw, err := dialect.NewReadWriter(ardupilotmega.Dialect)
	if err != nil {
		return nil, err
	}
	return &relay{in: in, send: send, publish: publish, now: time.Now, rw: rw, writers: map[registry.ID]*droneWriter{}}, nil
}

// handle deals with one message from command.<drone_id>.
func (r *relay) handle(data []byte) {
	var c command.Command
	if err := json.Unmarshal(data, &c); err != nil {
		log.Printf("relay: bad command: %v", err)
		return
	}
	if !r.now().Before(c.ExpiresAt) {
		r.expired.Add(1)
		r.report(c, command.Expired)
		return
	}
	target, ok := r.in.target(registry.ID(c.DroneID))
	if !ok {
		r.unreachable.Add(1)
		r.report(c, command.Unreachable)
		return
	}
	m, err := c.Long(target.sysID, target.compID)
	if err != nil {
		log.Printf("relay: command %s: %v", c.CommandID, err)
		r.unreachable.Add(1)
		r.report(c, command.Unreachable)
		return
	}
	w, err := r.writer(registry.ID(c.DroneID), target.src)
	if err == nil {
		err = w.w.WriteMessage(m)
	}
	if err != nil {
		log.Printf("relay: command %s: %v", c.CommandID, err)
		r.unreachable.Add(1)
		r.report(c, command.Unreachable)
		return
	}
	r.sent.Add(1)
	r.report(c, command.Sent)
}

// writer returns a drone's writer, pointed at its current address.
func (r *relay) writer(id registry.ID, to netip.AddrPort) (*droneWriter, error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if d, ok := r.writers[id]; ok {
		d.to = to // the drone may have been rebound
		return d, nil
	}
	d := &droneWriter{r: r, to: to}
	w, err := frame.NewWriter(frame.WriterConf{
		Writer:         d,
		DialectRW:      r.rw,
		OutVersion:     frame.V2,
		OutSystemID:    command.SenderSystem,
		OutComponentID: command.SenderComponent,
	})
	if err != nil {
		return nil, err
	}
	d.w = w
	r.writers[id] = d
	return d, nil
}

func (r *relay) report(c command.Command, status string) {
	r.reply(command.Reply{DroneID: c.DroneID, CommandID: c.CommandID, Attempt: c.Attempt, Status: status})
}

func (r *relay) reply(rep command.Reply) {
	data, err := json.Marshal(rep)
	if err == nil {
		err = r.publish(command.ReplySubject(rep.DroneID), data)
	}
	if err != nil {
		log.Printf("relay: reply: %v", err)
	}
}

// forward passes on a drone's answer to a command.
func (r *relay) forward(id registry.ID, m message.Message) {
	if ack, ok := m.(*ardupilotmega.MessageCommandAck); ok {
		r.reply(command.Reply{DroneID: int64(id), Ack: &command.Ack{
			Command: uint16(ack.Command), Result: uint8(ack.Result), Progress: ack.Progress,
		}})
	}
}
