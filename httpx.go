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
		token := s.accessToken(r)
		if token == "" {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		claimsUser, err := s.tokens.Parse(token)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		// Re-load from DB so deleted / deactivated users and role changes apply immediately.
		dbUser, err := s.users.FindByID(r.Context(), claimsUser.ID)
		if err != nil {
			writeError(w, http.StatusUnauthorized, "invalid or expired token")
			return
		}
		if !dbUser.IsActive {
			s.authCookie.clear(w)
			writeError(w, http.StatusForbidden, "account is inactive")
			return
		}
		if !isValidRole(dbUser.Role) {
			writeError(w, http.StatusForbidden, "account role is invalid")
			return
		}
		auth := dbUser.Auth()
		next(w, r.WithContext(withUser(r.Context(), &auth)))
	})
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
