package main

import (
	"encoding/json"
	"net/http"
	"strings"
)

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

func firstNonEmpty(values ...string) string {
	for _, v := range values {
		if strings.TrimSpace(v) != "" {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func writeError(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

func writeValidationError(w http.ResponseWriter, v *ValidationError) {
	writeJSON(w, http.StatusBadRequest, map[string]any{
		"error":  "validation failed",
		"errors": v.Errors,
	})
}

func (s *Server) requireAuth(next http.HandlerFunc) http.HandlerFunc {
	return withCORS(func(w http.ResponseWriter, r *http.Request) {
		dbUser, claims, status, msg := s.loadAuthedUser(r)
		if status != 0 {
			writeError(w, status, msg)
			return
		}
		auth := dbUser.Auth()
		auth.SessionID = claims.SessionID
		next(w, r.WithContext(withUser(r.Context(), &auth)))
	})
}

func clientPresentedSessionID(r *http.Request) string {
	if id := strings.TrimSpace(r.Header.Get(sessionHeaderName)); id != "" {
		return id
	}
	return strings.TrimSpace(r.URL.Query().Get(sessionQueryName))
}

// loadAuthedUser validates the cookie/Bearer JWT against the live single session.
// status is 0 on success.
func (s *Server) loadAuthedUser(r *http.Request) (*UserRecord, *AuthUser, int, string) {
	token := s.accessToken(r)
	if token == "" {
		return nil, nil, http.StatusUnauthorized, "authentication required"
	}
	claimsUser, err := s.tokens.Parse(token)
	if err != nil {
		return nil, nil, http.StatusUnauthorized, "invalid or expired token"
	}
	dbUser, err := s.users.FindByID(r.Context(), claimsUser.ID)
	if err != nil {
		return nil, nil, http.StatusUnauthorized, "invalid or expired token"
	}
	if !dbUser.IsActive {
		return nil, nil, http.StatusForbidden, "account is inactive"
	}
	if !isValidRole(dbUser.Role) {
		return nil, nil, http.StatusForbidden, "account role is invalid"
	}
	if !dbUser.sessionMatches(claimsUser.SessionID) {
		return nil, nil, http.StatusUnauthorized, errSignedInElsewhere
	}
	if presented := clientPresentedSessionID(r); presented != "" && presented != claimsUser.SessionID {
		return nil, nil, http.StatusUnauthorized, errSignedInElsewhere
	}
	return dbUser, claimsUser, 0, ""
}

func (s *Server) requireRole(roles ...string) func(http.HandlerFunc) http.HandlerFunc {
	allowed := map[string]struct{}{}
	for _, role := range roles {
		allowed[role] = struct{}{}
	}
	return func(next http.HandlerFunc) http.HandlerFunc {
		return s.requireAuth(func(w http.ResponseWriter, r *http.Request) {
			user, ok := userFromContext(r.Context())
			if !ok {
				writeError(w, http.StatusUnauthorized, "authentication required")
				return
			}
			if _, ok := allowed[user.Role]; !ok {
				writeError(w, http.StatusForbidden, "insufficient permissions")
				return
			}
			next(w, r)
		})
	}
}
