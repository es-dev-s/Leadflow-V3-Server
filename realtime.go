package main

import (
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"sync"
	"time"

	"github.com/google/uuid"
)

// Realtime event types broadcast to connected clients over SSE.
const (
	EvtLeadCreated  = "lead.created"
	EvtLeadUpdated  = "lead.updated"
	EvtLeadDeleted  = "lead.deleted"
	EvtUserCreated  = "user.created"
	EvtUserUpdated  = "user.updated"
	EvtUserDeleted  = "user.deleted"
	EvtNotification = "notification"
)

// RealtimeEvent is a small, cheap payload. Clients coalesce these and refresh
// the affected surface (list / notifications) rather than trusting the payload
// as the source of truth — keeps us correct under 200+ concurrent sessions.
type RealtimeEvent struct {
	Type    string `json:"type"`
	LeadID  string `json:"leadId,omitempty"`
	UserID  string `json:"userId,omitempty"`
	ActorID string `json:"actorId,omitempty"`
	At      int64  `json:"at"`
}

type sseClient struct {
	id     string
	userID string
	ch     chan []byte
}

// RealtimeHub fans out events to all live SSE subscribers. Sends are
// non-blocking: a slow/stuck client drops events instead of stalling the hub.
type RealtimeHub struct {
	mu      sync.RWMutex
	clients map[string]*sseClient
}

func NewRealtimeHub() *RealtimeHub {
	return &RealtimeHub{clients: make(map[string]*sseClient)}
}

func (h *RealtimeHub) add(c *sseClient) int {
	h.mu.Lock()
	defer h.mu.Unlock()
	h.clients[c.id] = c
	return len(h.clients)
}

func (h *RealtimeHub) remove(id string) {
	h.mu.Lock()
	c, ok := h.clients[id]
	if ok {
		delete(h.clients, id)
	}
	h.mu.Unlock()
	if ok {
		close(c.ch)
	}
}

func (h *RealtimeHub) clientCount() int {
	h.mu.RLock()
	defer h.mu.RUnlock()
	return len(h.clients)
}

// Broadcast delivers to every connected client.
func (h *RealtimeHub) Broadcast(evt RealtimeEvent) {
	if h == nil {
		return
	}
	if evt.At == 0 {
		evt.At = time.Now().UnixMilli()
	}
	payload, err := json.Marshal(evt)
	if err != nil {
		return
	}
	h.mu.RLock()
	for _, c := range h.clients {
		select {
		case c.ch <- payload:
		default:
			// Client buffer full — drop this event for that client; its next
			// refresh (heartbeat / focus / another event) reconciles state.
		}
	}
	h.mu.RUnlock()
}

// emitLead is a convenience broadcaster used by lead mutation handlers.
// Any lead mutation also invalidates cached aggregates so clients never see
// stale summaries after a change.
func (s *Server) emitLead(evtType, leadID, actorID string) {
	if s.respCache != nil {
		s.respCache.Clear()
	}
	if s.hub == nil {
		return
	}
	s.hub.Broadcast(RealtimeEvent{Type: evtType, LeadID: leadID, ActorID: actorID})
}

// emitUser is a convenience broadcaster used by user mutation handlers.
func (s *Server) emitUser(evtType, userID, actorID string) {
	if s.respCache != nil {
		s.respCache.Clear()
	}
	if s.hub == nil {
		return
	}
	s.hub.Broadcast(RealtimeEvent{Type: evtType, UserID: userID, ActorID: actorID})
}

// handleEvents serves the SSE stream. Browsers authenticate via the HttpOnly
// cookie (same-origin / credentials). API clients may send Authorization.
// Query-string tokens are rejected.
func (s *Server) handleEvents(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	token := s.accessToken(r)
	if token == "" {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	claimsUser, err := s.tokens.Parse(token)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Match requireAuth: deleted / deactivated / role-changed users drop off.
	dbUser, err := s.users.FindByID(r.Context(), claimsUser.ID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !dbUser.IsActive {
		writeError(w, http.StatusForbidden, "account is inactive")
		return
	}
	if !isValidRole(dbUser.Role) {
		writeError(w, http.StatusForbidden, "account role is invalid")
		return
	}

	flusher, ok := w.(http.Flusher)
	if !ok {
		writeError(w, http.StatusInternalServerError, "streaming unsupported")
		return
	}

	// Disable the server WriteTimeout for this long-lived connection.
	rc := http.NewResponseController(w)
	_ = rc.SetWriteDeadline(time.Time{})

	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.Header().Set("Connection", "keep-alive")
	// Disable proxy buffering (nginx / Next) so events flush immediately.
	w.Header().Set("X-Accel-Buffering", "no")
	w.WriteHeader(http.StatusOK)

	client := &sseClient{
		id:     uuid.NewString(),
		userID: dbUser.ID,
		ch:     make(chan []byte, 64),
	}
	total := s.hub.add(client)
	defer s.hub.remove(client.id)
	log.Printf("sse: client connected user=%s total=%d", dbUser.ID, total)

	// Advise the browser to reconnect quickly and confirm the stream is live.
	fmt.Fprintf(w, "retry: 3000\nevent: ready\ndata: {\"ok\":true}\n\n")
	flusher.Flush()

	heartbeat := time.NewTicker(20 * time.Second)
	defer heartbeat.Stop()
	ctx := r.Context()

	for {
		select {
		case <-ctx.Done():
			return
		case <-heartbeat.C:
			if _, err := fmt.Fprint(w, ": ping\n\n"); err != nil {
				return
			}
			flusher.Flush()
		case msg, ok := <-client.ch:
			if !ok {
				return
			}
			if _, err := fmt.Fprintf(w, "data: %s\n\n", msg); err != nil {
				return
			}
			flusher.Flush()
		}
	}
}
