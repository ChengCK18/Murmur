package main

import (
	"context"
	"encoding/json"
	"log"
	"net/http"

	"github.com/nats-io/nats.go"

	"github.com/ChengCK18/murmur/internal/alert"
)

// alertMsg is {"type":"alert","alert":{...}}, and geofenceMsg is
// {"type":"geofence","fence":{...}}. Every browser gets both: alerts are about
// the whole fleet.
type alertMsg struct {
	Type  string      `json:"type"`
	Alert alert.Alert `json:"alert"`
}

type geofenceMsg struct {
	Type  string      `json:"type"`
	Fence alert.Fence `json:"fence"`
}

// broadcast queues a message for every browser.
func (h *hub) broadcast(b []byte) {
	h.mu.Lock()
	defer h.mu.Unlock()
	for cl := range h.clients {
		cl.mu.Lock()
		cl.queue = append(cl.queue, b)
		cl.mu.Unlock()
		cl.poke()
	}
}

func (h *hub) addClient(cl *client) {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[cl] = struct{}{}
}

// runAlerts passes alerts and geofence changes to browsers. They are on core
// NATS: a browser that reconnects reloads both.
func runAlerts(nc *nats.Conn, h *hub) error {
	if _, err := nc.Subscribe(alert.Subjects, func(m *nats.Msg) {
		var a alert.Alert
		if err := json.Unmarshal(m.Data, &a); err != nil {
			log.Printf("bad alert: %v", err)
			return
		}
		h.broadcast(encode(alertMsg{Type: "alert", Alert: a}))
	}); err != nil {
		return err
	}
	_, err := nc.Subscribe(alert.GeofenceChanged, func(m *nats.Msg) {
		var f alert.Fence
		if err := json.Unmarshal(m.Data, &f); err != nil {
			log.Printf("bad geofence: %v", err)
			return
		}
		h.broadcast(encode(geofenceMsg{Type: "geofence", Fence: f}))
	})
	return err
}

// ask sends a request to the alerter and writes its answer, as is.
func (s *server) ask(w http.ResponseWriter, r *http.Request, subject string, v any) {
	data, err := json.Marshal(v)
	if err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not ask"})
		return
	}
	wait, cancel := context.WithTimeout(r.Context(), commanderWait)
	defer cancel()
	m, err := s.nc.RequestWithContext(wait, subject, data)
	if err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "the alerter is not answering"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Write(m.Data) //nolint:errcheck
}

// handleAlerts serves GET /api/alerts: the alerts raised now.
func (s *server) handleAlerts(w http.ResponseWriter, r *http.Request) {
	s.ask(w, r, alert.ActiveSubject, struct{}{})
}

// handleGeofence serves GET /api/geofence.
func (s *server) handleGeofence(w http.ResponseWriter, r *http.Request) {
	s.ask(w, r, alert.GeofenceGet, struct{}{})
}

// handleSetGeofence serves PUT /api/geofence with {"polygon":[...]}; an empty
// polygon removes the geofence. It takes a login, like a command.
func (s *server) handleSetGeofence(w http.ResponseWriter, r *http.Request) {
	user := s.sessions.userOf(r.Context(), cookieToken(r))
	if user == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "log in to change the geofence"})
		return
	}
	var body struct {
		Polygon []alert.Point `json:"polygon"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 64<<10)).Decode(&body); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	s.ask(w, r, alert.GeofenceSet, alert.SetFence{Polygon: body.Polygon, User: user})
}
