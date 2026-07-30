package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

// TelemetryEmitter sends CRM ops events to the isolated telemetry service
// without blocking request handlers. No-ops when TELEMETRY_URL is unset.
type TelemetryEmitter struct {
	baseURL   string
	ingestKey string
	client    *http.Client
	ch        chan telemetryPayload
	once      sync.Once
}

type telemetryPayload struct {
	Events []map[string]any `json:"events"`
}

func NewTelemetryEmitter(baseURL, ingestKey string) *TelemetryEmitter {
	baseURL = strings.TrimRight(strings.TrimSpace(baseURL), "/")
	if baseURL == "" {
		return &TelemetryEmitter{}
	}
	e := &TelemetryEmitter{
		baseURL:   baseURL,
		ingestKey: strings.TrimSpace(ingestKey),
		client:    &http.Client{Timeout: 2 * time.Second},
		ch:        make(chan telemetryPayload, 512),
	}
	e.once.Do(func() { go e.loop() })
	log.Printf("telemetry emitter enabled → %s", baseURL)
	return e
}

func (e *TelemetryEmitter) Enabled() bool {
	return e != nil && e.baseURL != "" && e.ch != nil
}

func (e *TelemetryEmitter) Emit(events ...map[string]any) {
	if !e.Enabled() || len(events) == 0 {
		return
	}
	payload := telemetryPayload{Events: events}
	select {
	case e.ch <- payload:
	default:
		// Drop under pressure — never block CRM.
	}
}

func (e *TelemetryEmitter) EmitOne(kind, severity, message, path, method string, statusCode int, userID string) {
	ev := map[string]any{
		"kind":     kind,
		"severity": severity,
		"source":   "crm",
		"message":  message,
		"path":     path,
		"method":   method,
	}
	if statusCode > 0 {
		ev["statusCode"] = statusCode
	}
	if userID != "" {
		ev["userId"] = userID
	}
	e.Emit(ev)
}

func (e *TelemetryEmitter) loop() {
	for payload := range e.ch {
		e.flush(payload)
	}
}

func (e *TelemetryEmitter) flush(payload telemetryPayload) {
	body, err := json.Marshal(payload)
	if err != nil {
		return
	}
	req, err := http.NewRequest(http.MethodPost, e.baseURL+"/v1/ingest", bytes.NewReader(body))
	if err != nil {
		return
	}
	req.Header.Set("Content-Type", "application/json")
	if e.ingestKey != "" {
		req.Header.Set("X-Telemetry-Token", e.ingestKey)
	}
	res, err := e.client.Do(req)
	if err != nil {
		return
	}
	_ = res.Body.Close()
}

// statusCapture wraps ResponseWriter to observe the final status code.
type statusCapture struct {
	http.ResponseWriter
	status int
	wrote  bool
}

func (s *statusCapture) WriteHeader(code int) {
	if !s.wrote {
		s.status = code
		s.wrote = true
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusCapture) Write(b []byte) (int, error) {
	if !s.wrote {
		s.status = http.StatusOK
		s.wrote = true
	}
	return s.ResponseWriter.Write(b)
}

func (s *statusCapture) Flush() {
	if f, ok := s.ResponseWriter.(http.Flusher); ok {
		f.Flush()
	}
}

func telemetryMiddleware(emitter *TelemetryEmitter, next http.Handler) http.Handler {
	if emitter == nil || !emitter.Enabled() {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		// Skip noisy/long-lived paths.
		path := r.URL.Path
		if path == "/health" || path == "/api/health" || path == "/api/events" {
			next.ServeHTTP(w, r)
			return
		}
		cap := &statusCapture{ResponseWriter: w, status: http.StatusOK}
		next.ServeHTTP(cap, r)
		if cap.status < 400 {
			return
		}
		userID := ""
		if u, ok := userFromContext(r.Context()); ok {
			userID = u.ID
		}
		sev := "warn"
		if cap.status >= 500 {
			sev = "error"
		}
		emitter.EmitOne("http_status", sev, http.StatusText(cap.status), path, r.Method, cap.status, userID)
	})
}
