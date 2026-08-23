package main

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

func (s *Server) applyLivePresence(users []PublicUser) {
	if s.hub == nil {
		return
	}
	for i := range users {
		users[i].IsActiveSession = s.hub.IsOnline(users[i].ID)
	}
}

func (s *Server) applyLivePresenceOne(user *PublicUser) {
	if user == nil || s.hub == nil {
		return
	}
	user.IsActiveSession = s.hub.IsOnline(user.ID)
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dbStatus := "ok"
	if err := s.poolPing(r.Context()); err != nil {
		dbStatus = "unreachable"
	}
	status := "ok"
	if dbStatus != "ok" {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   status,
		"service":  "leadflow-backend",
		"database": dbStatus,
		"time":     time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req LoginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	v := &ValidationError{}
	req.Email = requireEmail(v, "email", req.Email)
	// Login accepts any non-empty password length; creation still enforces strength.
	if strings.TrimSpace(req.Password) == "" {
		v.Add("password", "password is required")
	} else if utf8.RuneCountInString(req.Password) > 128 {
		v.Add("password", "password must be at most 128 characters")
	}
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}

	ip := clientIP(r)
	gateKey := ip + "|" + strings.ToLower(req.Email)

	user, err := s.users.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		// Count failures only so concurrent valid sessions are never rate-limited.
		if s.loginGate != nil && !s.loginGate.allow(gateKey) {
			writeError(w, http.StatusTooManyRequests, "too many login attempts — try again shortly")
			return
		}
		switch {
		case errors.Is(err, errInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "invalid email or password")
		case errors.Is(err, errAccountInactive):
			writeError(w, http.StatusForbidden, "account is inactive — contact an administrator")
		case errors.Is(err, errPasswordNotSet):
			writeError(w, http.StatusForbidden, "password not set — ask a superadmin to set one")
		default:
			log.Printf("login error: %v", err)
			writeError(w, http.StatusInternalServerError, "login failed")
		}
		return
	}

	s.issueLiveSession(w, r, user, true)
}

// handleClaimSession mints a new sole session for this browser tab when it
// already has a valid cookie but no per-tab session id (new window / refresh).
func (s *Server) handleClaimSession(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	user, err := s.users.FindByID(r.Context(), authUser.ID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user no longer exists")
		return
	}
	s.issueLiveSession(w, r, user, false)
}

// issueLiveSession replaces the user's sole live session, sets the auth
// cookie, and kicks every other SSE connection for this account.
func (s *Server) issueLiveSession(w http.ResponseWriter, r *http.Request, user *UserRecord, returnBearer bool) {
	token, jti, expires, err := s.tokens.Issue(user.Auth())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue token")
		return
	}
	if err := s.users.ReplaceActiveSession(r.Context(), user.ID, jti); err != nil {
		log.Printf("replace active session: %v", err)
		writeError(w, http.StatusInternalServerError, "could not issue token")
		return
	}
	if s.hub != nil {
		s.hub.BroadcastToUser(user.ID, RealtimeEvent{
			Type:      EvtAuthSessionReplaced,
			UserID:    user.ID,
			SessionID: jti,
		})
		s.hub.DropUserExcept(user.ID, jti)
	}

	s.authCookie.set(w, token, expires)

	resp := AuthResponse{
		ExpiresAt: expires,
		User:      user.Public(),
		SessionID: jti,
	}
	// Keep Bearer token in the body for loadtest/CLI; browsers ignore it and
	// use the HttpOnly cookie instead.
	if returnBearer && strings.EqualFold(envOr("LOGIN_RETURN_TOKEN", "true"), "true") {
		resp.Token = token
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	token := s.accessToken(r)
	presented := clientPresentedSessionID(r)
	if token == "" {
		s.authCookie.clear(w)
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	claims, err := s.tokens.Parse(token)
	if err != nil {
		writeJSON(w, http.StatusOK, map[string]any{"ok": true})
		return
	}
	jwtIsLive := false
	if dbUser, findErr := s.users.FindByID(r.Context(), claims.ID); findErr == nil {
		jwtIsLive = dbUser.sessionMatches(claims.SessionID)
	}
	clearLive, clearCookie := logoutAction(presented, claims.SessionID, jwtIsLive)
	if clearLive && claims.SessionID != "" {
		if err := s.users.ClearActiveSessionIfCurrent(r.Context(), claims.ID, claims.SessionID); err != nil {
			log.Printf("clear active session: %v", err)
		}
		if s.hub != nil {
			s.hub.DropUser(claims.ID)
		}
	}
	if clearCookie {
		s.authCookie.clear(w)
	}
	writeJSON(w, http.StatusOK, map[string]any{"ok": true})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	user, err := s.users.FindByID(r.Context(), authUser.ID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user no longer exists")
		return
	}
	pub := user.Public()
	s.applyLivePresenceOne(&pub)
	writeJSON(w, http.StatusOK, map[string]any{"user": pub})
}

func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Create/edit pickers get only roles this actor may assign.
	writeJSON(w, http.StatusOK, map[string]any{
		"roles":     listCreatableRoles(authUser.Role),
		"allRoles":  listRoles(),
		"canManage": canManageUsers(authUser.Role),
	})
}
