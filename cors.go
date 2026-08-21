package main

import (
	"net/http"
	"strings"
)

// CORSConfig is an explicit origin allowlist. Reflected Origin is never used.
type CORSConfig struct {
	allowed map[string]struct{}
}

func loadCORSConfig() *CORSConfig {
	raw := envOr("CORS_ORIGINS", "")
	cfg := &CORSConfig{allowed: map[string]struct{}{}}
	for _, part := range strings.Split(raw, ",") {
		origin := strings.TrimSpace(part)
		if origin == "" {
			continue
		}
		cfg.allowed[strings.TrimRight(origin, "/")] = struct{}{}
	}
	return cfg
}

func (c *CORSConfig) allows(origin string) bool {
	if c == nil || origin == "" {
		return false
	}
	_, ok := c.allowed[strings.TrimRight(origin, "/")]
	return ok
}

// applyCORS writes CORS headers when the request Origin is allowlisted.
// Returns false when a disallowed Origin sent a preflight (caller should stop).
func (c *CORSConfig) applyCORS(w http.ResponseWriter, r *http.Request) bool {
	origin := strings.TrimSpace(r.Header.Get("Origin"))
	if origin == "" {
		return true
	}
	if !c.allows(origin) {
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusForbidden)
			return false
		}
		// Actual request: omit ACAO so the browser blocks credentialed reads.
		return true
	}

	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-LeadFlow-Session")
	w.Header().Set("Access-Control-Max-Age", "86400")
	return true
}

func withCORS(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if !corsConfig.applyCORS(w, r) {
			return
		}
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next(w, r)
	}
}

// Package-level config wired from main after loadEnvFile.
var corsConfig = &CORSConfig{allowed: map[string]struct{}{}}
