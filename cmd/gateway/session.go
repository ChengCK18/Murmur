package main

import (
	"context"
	"crypto/rand"
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"time"

	"github.com/redis/go-redis/v9"
)

// sessionCookie holds a random token; Redis maps it to a user. Redis rather
// than the gateway's memory, so a gateway restart logs nobody out (plan open
// item 6: expires after 8 hours, WebSocket on the same origin).
const (
	sessionCookie = "murmur_session"
	sessionLife   = 8 * time.Hour
)

// sessions is the demo login: one user, set by flags. Real authentication is
// stage E of the plan.
type sessions struct {
	rdb            *redis.Client
	user, password string
}

func sessionKey(token string) string { return "session:" + token }

// userOf returns who a token belongs to, or "".
func (s sessions) userOf(ctx context.Context, token string) string {
	if token == "" {
		return ""
	}
	user, err := s.rdb.Get(ctx, sessionKey(token)).Result()
	if err != nil {
		return "" // expired, logged out or Redis down: no commands either way
	}
	return user
}

func cookieToken(r *http.Request) string {
	c, err := r.Cookie(sessionCookie)
	if err != nil {
		return ""
	}
	return c.Value
}

func setCookie(w http.ResponseWriter, r *http.Request, token string, life time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name: sessionCookie, Value: token, Path: "/", MaxAge: int(life.Seconds()),
		HttpOnly: true, SameSite: http.SameSiteStrictMode, Secure: r.TLS != nil,
	})
}

// handleLogin serves POST /api/login with {"username":"...","password":"..."}.
func (s *server) handleLogin(w http.ResponseWriter, r *http.Request) {
	var creds struct {
		Username string `json:"username"`
		Password string `json:"password"`
	}
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&creds); err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": "bad request"})
		return
	}
	userOK := subtle.ConstantTimeCompare([]byte(creds.Username), []byte(s.sessions.user)) == 1
	passOK := subtle.ConstantTimeCompare([]byte(creds.Password), []byte(s.sessions.password)) == 1
	if !userOK || !passOK || s.sessions.password == "" {
		writeJSON(w, http.StatusUnauthorized, map[string]string{"error": "wrong username or password"})
		return
	}
	var b [32]byte
	if _, err := rand.Read(b[:]); err != nil {
		writeJSON(w, http.StatusInternalServerError, map[string]string{"error": "could not log in"})
		return
	}
	token := hex.EncodeToString(b[:])
	if err := s.sessions.rdb.Set(r.Context(), sessionKey(token), creds.Username, sessionLife).Err(); err != nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "could not log in"})
		return
	}
	setCookie(w, r, token, sessionLife)
	writeJSON(w, http.StatusOK, map[string]string{"user": creds.Username})
}

// handleLogout serves POST /api/logout.
func (s *server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if token := cookieToken(r); token != "" {
		if err := s.sessions.rdb.Del(r.Context(), sessionKey(token)).Err(); err != nil && !errors.Is(err, redis.Nil) {
			writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "could not log out"})
			return
		}
	}
	setCookie(w, r, "", -time.Second)
	writeJSON(w, http.StatusOK, map[string]string{})
}

// handleSession serves GET /api/session: who is logged in, if anyone.
func (s *server) handleSession(w http.ResponseWriter, r *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"user": s.sessions.userOf(r.Context(), cookieToken(r))})
}
