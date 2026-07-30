package main

import (
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"
)

type rateBucket struct {
	count   int
	resetAt time.Time
}

// loginLimiter is a simple in-memory limiter for brute-force protection.
type loginLimiter struct {
	mu      sync.Mutex
	buckets map[string]*rateBucket
	limit   int
	window  time.Duration
}

func newLoginLimiter(limit int, window time.Duration) *loginLimiter {
	return &loginLimiter{
		buckets: make(map[string]*rateBucket),
		limit:   limit,
		window:  window,
	}
}

func (l *loginLimiter) allow(key string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	l.pruneLocked(now)

	b, ok := l.buckets[key]
	if !ok || now.After(b.resetAt) {
		l.buckets[key] = &rateBucket{count: 1, resetAt: now.Add(l.window)}
		return true
	}
	if b.count >= l.limit {
		return false
	}
	b.count++
	return true
}

func (l *loginLimiter) pruneLocked(now time.Time) {
	// Opportunistic prune keeps memory bounded under many concurrent IPs/emails.
	if len(l.buckets) < 256 {
		return
	}
	for k, b := range l.buckets {
		if now.After(b.resetAt) {
			delete(l.buckets, k)
		}
	}
}

func clientIP(r *http.Request) string {
	// Only trust forwarded headers when explicitly behind a reverse proxy.
	if strings.EqualFold(strings.TrimSpace(os.Getenv("TRUST_PROXY")), "1") ||
		strings.EqualFold(strings.TrimSpace(os.Getenv("TRUST_PROXY")), "true") {
		if xff := r.Header.Get("X-Forwarded-For"); xff != "" {
			parts := strings.Split(xff, ",")
			return strings.TrimSpace(parts[0])
		}
		if xr := r.Header.Get("X-Real-IP"); xr != "" {
			return strings.TrimSpace(xr)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}
