package main

import (
	"encoding/json"
	"io"
	"net/http"
	"strconv"
	"strings"
	"time"
)

type Server struct {
	store     *Store
	tokens    *TokenService
	presence  *Presence
	poller    *HealthPoller
	ingestKey string
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return withCORS(func(w http.ResponseWriter, r *http.Request) {
		token := accessToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		user, err := s.tokens.Parse(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		next(w, r.WithContext(withUser(r.Context(), user)))
	})
}

func (s *Server) requireSupportRead(next http.HandlerFunc) http.HandlerFunc {
	return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
		user, ok := userFromContext(r.Context())
		if !ok || !canReadSupport(user.Role) {
			writeError(w, http.StatusForbidden, "support analytics is not available for this role")
			return
		}
		next(w, r)
	})
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"status":  "ok",
		"service": "leadflow-telemetry",
		"time":    nowUTC().Format(time.RFC3339),
		"crm":     s.poller.CurrentStatus(),
	})
}

func (s *Server) authorizeIngest(r *http.Request) (*AuthUser, bool) {
	if key := strings.TrimSpace(r.Header.Get("X-Telemetry-Token")); key != "" && s.ingestKey != "" && key == s.ingestKey {
		return nil, true
	}
	token := accessToken(r)
	if token == "" {
		return nil, false
	}
	user, err := s.tokens.Parse(token)
	if err != nil {
		return nil, false
	}
	return user, true
}

func (s *Server) handleIngest(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, ok := s.authorizeIngest(r)
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	if err != nil {
		writeError(w, http.StatusBadRequest, "invalid body")
		return
	}
	var payload struct {
		Events []IngestEvent `json:"events"`
	}
	if err := json.Unmarshal(body, &payload); err != nil {
		// allow single event
		var one IngestEvent
		if err2 := json.Unmarshal(body, &one); err2 != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON")
			return
		}
		payload.Events = []IngestEvent{one}
	}
	if len(payload.Events) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{"accepted": 0})
		return
	}
	if len(payload.Events) > 100 {
		payload.Events = payload.Events[:100]
	}

	for i := range payload.Events {
		ev := &payload.Events[i]
		if user != nil {
			if strings.TrimSpace(ev.UserID) == "" {
				ev.UserID = user.ID
			}
			if strings.TrimSpace(ev.UserEmail) == "" {
				ev.UserEmail = user.Email
			}
			if strings.TrimSpace(ev.Source) == "" {
				ev.Source = "ui"
			}
		} else if strings.TrimSpace(ev.Source) == "" {
			ev.Source = "crm"
		}
		ev.Kind = normalizeKind(ev.Kind)
		if ev.Severity == "" {
			ev.Severity = severityFor(ev)
		}
	}

	n, err := s.store.InsertEvents(r.Context(), payload.Events)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to store events")
		return
	}
	writeJSON(w, http.StatusAccepted, map[string]any{"accepted": n})
}

func normalizeKind(kind string) string {
	k := strings.ToLower(strings.TrimSpace(kind))
	switch k {
	case "http_status", "http_error", "client_error", "connection_break",
		"health_change", "server_start", "server_stop", "server_panic", "session":
		if k == "http_error" {
			return "http_status"
		}
		return k
	default:
		if k == "" {
			return "client_error"
		}
		return k
	}
}

func severityFor(ev *IngestEvent) string {
	if ev.StatusCode != nil {
		if *ev.StatusCode >= 500 {
			return "error"
		}
		if *ev.StatusCode >= 400 {
			return "warn"
		}
	}
	switch ev.Kind {
	case "server_panic", "server_stop", "connection_break":
		return "error"
	case "health_change":
		return "warn"
	default:
		return "info"
	}
}

func (s *Server) handleHeartbeat(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	user, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	var req struct {
		SessionID string `json:"sessionId"`
	}
	_ = json.NewDecoder(io.LimitReader(r.Body, 4096)).Decode(&req)
	if err := s.presence.Touch(r.Context(), user.ID, req.SessionID); err != nil {
		writeError(w, http.StatusInternalServerError, "failed to record presence")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	active, concurrent, err := s.presence.Count(r.Context())
	if err != nil {
		active, concurrent = 0, 0
	}
	overview, err := s.store.Overview(r.Context(), active, concurrent, s.poller.CurrentStatus())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to load overview")
		return
	}
	writeJSON(w, http.StatusOK, overview)
}

func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	limit := 50
	if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	var before *time.Time
	if raw := strings.TrimSpace(r.URL.Query().Get("before")); raw != "" {
		if t, err := time.Parse(time.RFC3339, raw); err == nil {
			before = &t
		}
	}
	items, err := s.store.ListEvents(r.Context(), limit, before)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to list events")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"items": items})
}
