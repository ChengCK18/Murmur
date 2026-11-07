package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"
	"strconv"
	"time"

	"github.com/nats-io/nats.go"

	"github.com/ChengCK18/murmur/internal/command"
)

const (
	// commanderWait is how long the commander has to answer. Past it the
	// browser is told, rather than being left waiting.
	commanderWait = 2 * time.Second
	// recentCommands is how many a browser reloads per drone.
	recentCommands = 20
	// maxCommandsAtOnce limits what one browser can have waiting on the
	// commander, so it cannot pile up goroutines here.
	maxCommandsAtOnce = 64
)

// statusMsg is {"type":"command_status","request_id":"...","update":{...}}.
// RequestID ties the first update to the browser's request, before the
// browser knows the command's ID.
type statusMsg struct {
	Type      string         `json:"type"`
	RequestID string         `json:"request_id,omitempty"`
	Update    command.Update `json:"update"`
}

// status queues a command status for the browser.
func (c *client) status(requestID string, u command.Update) {
	c.mu.Lock()
	c.queue = append(c.queue, encode(statusMsg{Type: "command_status", RequestID: requestID, Update: u}))
	c.mu.Unlock()
	c.poke()
}

// fleetMsg is {"type":"fleet_started","request_id":"...","fleet_id":"...","count":20}.
type fleetMsg struct {
	Type      string `json:"type"`
	RequestID string `json:"request_id,omitempty"`
	command.FleetStarted
}

func (c *client) fleet(requestID string, f command.FleetStarted) {
	c.mu.Lock()
	c.queue = append(c.queue, encode(fleetMsg{Type: "fleet_started", RequestID: requestID, FleetStarted: f}))
	c.mu.Unlock()
	c.poke()
}

// commandStatus sends a status to the browsers that care: any logged in as the
// user who issued it, and any watching the drone (plan §4).
func (h *hub) commandStatus(u command.Update) {
	h.mu.Lock()
	defer h.mu.Unlock()
	to := map[*client]struct{}{}
	for cl := range h.users[u.IssuedBy] {
		to[cl] = struct{}{}
	}
	for cl := range h.subs[u.DroneID] {
		to[cl] = struct{}{}
	}
	for cl := range h.areas {
		if _, in := cl.inArea[u.DroneID]; in {
			to[cl] = struct{}{}
		}
	}
	for cl := range to {
		cl.status("", u)
	}
}

// addUser and removeUser track who is logged in on which connection.
func (h *hub) addUser(cl *client, user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	if h.users[user] == nil {
		h.users[user] = map[*client]struct{}{}
	}
	h.users[user][cl] = struct{}{}
}

func (h *hub) removeUser(cl *client, user string) {
	h.mu.Lock()
	defer h.mu.Unlock()
	delete(h.users[user], cl)
	if len(h.users[user]) == 0 {
		delete(h.users, user)
	}
}

// runStatus passes status updates from the commander to browsers. They are on
// core NATS: a gateway that was down reloads them through recentCommands.
func runStatus(nc *nats.Conn, h *hub) error {
	_, err := nc.Subscribe(command.StatusSubjects, func(m *nats.Msg) {
		var u command.Update
		if err := json.Unmarshal(m.Data, &u); err != nil {
			log.Printf("bad command status: %v", err)
			return
		}
		h.commandStatus(u)
	})
	return err
}

// command forwards one browser command to the commander. It checks the session
// on every command, not only at connect, so a logout or an expiry takes effect
// at once. Every path ends in a status for the browser: none stays pending.
func (s *server) command(ctx context.Context, cl *client, token string, req request) {
	refuse := func(reason string) {
		cl.status(req.RequestID, command.Update{DroneID: req.DroneID, Type: req.Command, Status: command.Rejected, Reason: reason, At: time.Now()})
	}
	user := s.sessions.userOf(ctx, token)
	if user == "" {
		refuse("log in to send commands")
		return
	}
	data, err := json.Marshal(command.Request{DroneID: req.DroneID, Type: req.Command, Params: req.Params, IssuedBy: user})
	if err != nil {
		refuse("could not send the command")
		return
	}
	wait, cancel := context.WithTimeout(ctx, commanderWait)
	defer cancel()
	m, err := s.nc.RequestWithContext(wait, command.RequestSubject, data)
	if err != nil {
		refuse("the command service is not answering")
		return
	}
	var u command.Update
	if err := json.Unmarshal(m.Data, &u); err != nil {
		refuse("the command service gave a bad answer")
		return
	}
	cl.status(req.RequestID, u)
}

// fleetCommand forwards one command for several drones. Each drone's statuses
// come back the usual way; the browser counts them against the total here.
func (s *server) fleetCommand(ctx context.Context, cl *client, token string, req request) {
	refuse := func(reason string) { cl.fleet(req.RequestID, command.FleetStarted{Error: reason}) }
	user := s.sessions.userOf(ctx, token)
	if user == "" {
		refuse("log in to send commands")
		return
	}
	data, err := json.Marshal(command.FleetRequest{DroneIDs: req.Drones, Type: req.Command, Params: req.Params, IssuedBy: user})
	if err != nil {
		refuse("could not send the command")
		return
	}
	wait, cancel := context.WithTimeout(ctx, commanderWait)
	defer cancel()
	m, err := s.nc.RequestWithContext(wait, command.FleetSubject, data)
	if err != nil {
		refuse("the command service is not answering")
		return
	}
	var f command.FleetStarted
	if err := json.Unmarshal(m.Data, &f); err != nil {
		refuse("the command service gave a bad answer")
		return
	}
	cl.fleet(req.RequestID, f)
}

// handleCommands serves GET /api/drones/{id}/commands: a drone's latest
// commands, which a reconnected browser reloads from the command service.
func (s *server) handleCommands(w http.ResponseWriter, r *http.Request) {
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad drone ID"})
		return
	}
	s.askRecent(w, r, command.RecentRequest{DroneID: id, Limit: recentCommands})
}

// handleFleet serves GET /api/fleets/{id}/commands: every drone's command of a
// fleet command, for the same reason.
func (s *server) handleFleet(w http.ResponseWriter, r *http.Request) {
	s.askRecent(w, r, command.RecentRequest{FleetID: r.PathValue("id")})
}

func (s *server) askRecent(w http.ResponseWriter, r *http.Request, req command.RecentRequest) {
	if s.sessions.userOf(r.Context(), cookieToken(r)) == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "log in to see commands"})
		return
	}
	data, err := json.Marshal(req)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not ask"})
		return
	}
	wait, cancel := context.WithTimeout(r.Context(), commanderWait)
	defer cancel()
	m, err := s.nc.RequestWithContext(wait, command.RecentSubject, data)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the command service is not answering"})
		return
	}
	var us []command.Update
	if err := json.Unmarshal(m.Data, &us); err != nil {
		writeJSON(w, http.StatusBadGateway, map[string]string{"error": "bad answer from the command service"})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"commands": us})
}
