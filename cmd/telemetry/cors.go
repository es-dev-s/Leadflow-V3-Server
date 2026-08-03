package main

import (
	"net/http"
	"strings"
)

type CORSConfig struct {
	allowed map[string]struct{}
}

func loadCORSConfig() *CORSConfig {
	cfg := &CORSConfig{allowed: map[string]struct{}{}}
	for _, part := range strings.Split(envOr("CORS_ORIGINS", ""), ",") {
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
		return true
	}
	w.Header().Set("Access-Control-Allow-Origin", origin)
	w.Header().Set("Access-Control-Allow-Credentials", "true")
	w.Header().Set("Vary", "Origin")
	w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
	w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Telemetry-Token")
	w.Header().Set("Access-Control-Max-Age", "86400")
	return true
}

var corsConfig = &CORSConfig{allowed: map[string]struct{}{}}

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
