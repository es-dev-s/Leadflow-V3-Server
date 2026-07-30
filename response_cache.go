package main

import (
	"encoding/json"
	"net/http"
	"sync"
	"time"
)

// ResponseCache is a small in-memory TTL cache for read-heavy aggregate
// endpoints (summary, buckets, geography, dashboard…). Entries are scoped per
// user (RBAC visibility differs) and the whole cache is cleared whenever any
// lead/user mutation happens, so realtime correctness is preserved — the TTL
// only bounds staleness from writes made outside this API.
type ResponseCache struct {
	mu      sync.RWMutex
	entries map[string]responseCacheEntry
}

type responseCacheEntry struct {
	data    []byte
	expires time.Time
}

const responseCacheMaxEntries = 4096

func NewResponseCache() *ResponseCache {
	return &ResponseCache{entries: make(map[string]responseCacheEntry)}
}

func (c *ResponseCache) Get(key string) ([]byte, bool) {
	c.mu.RLock()
	entry, ok := c.entries[key]
	c.mu.RUnlock()
	if !ok || time.Now().After(entry.expires) {
		return nil, false
	}
	return entry.data, true
}

func (c *ResponseCache) Set(key string, data []byte, ttl time.Duration) {
	if ttl <= 0 || len(data) == 0 {
		return
	}
	c.mu.Lock()
	if len(c.entries) >= responseCacheMaxEntries {
		// Simple full reset beats LRU bookkeeping at this scale.
		c.entries = make(map[string]responseCacheEntry)
	}
	c.entries[key] = responseCacheEntry{data: data, expires: time.Now().Add(ttl)}
	c.mu.Unlock()
}

func (c *ResponseCache) Clear() {
	c.mu.Lock()
	c.entries = make(map[string]responseCacheEntry)
	c.mu.Unlock()
}

func cacheKeyFor(r *http.Request) string {
	userID := ""
	if u, ok := userFromContext(r.Context()); ok {
		userID = u.ID
	}
	return r.URL.Path + "?" + r.URL.RawQuery + "|" + userID
}

// serveFromCache writes the cached payload if present. Returns true on a hit.
func (s *Server) serveFromCache(w http.ResponseWriter, r *http.Request) bool {
	if s.respCache == nil || r.Method != http.MethodGet {
		return false
	}
	data, ok := s.respCache.Get(cacheKeyFor(r))
	if !ok {
		return false
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("X-Cache", "hit")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
	return true
}

// writeCachedJSON marshals once, stores in the cache, and writes the response.
func (s *Server) writeCachedJSON(w http.ResponseWriter, r *http.Request, payload any, ttl time.Duration) {
	data, err := json.Marshal(payload)
	if err != nil {
		writeError(w, http.StatusInternalServerError, "failed to encode response")
		return
	}
	if s.respCache != nil && r.Method == http.MethodGet {
		s.respCache.Set(cacheKeyFor(r), data, ttl)
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write(data)
}
