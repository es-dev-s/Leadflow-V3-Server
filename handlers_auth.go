package main

import (
	"errors"
	"log"
	"net/http"
	"strings"
	"time"
	"unicode/utf8"
)

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

	token, expires, err := s.tokens.Issue(user.Auth())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue token")
		return
	}

	s.authCookie.set(w, token, expires)

	resp := AuthResponse{
		ExpiresAt: expires,
		User:      user.Public(),
	}
	// Keep Bearer token in the body for loadtest/CLI; browsers ignore it and
	// use the HttpOnly cookie instead.
	if strings.EqualFold(envOr("LOGIN_RETURN_TOKEN", "true"), "true") {
		resp.Token = token
	}
	writeJSON(w, http.StatusOK, resp)
}

func (s *Server) handleLogout(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	s.authCookie.clear(w)
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
	writeJSON(w, http.StatusOK, map[string]any{"user": user.Public()})
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
