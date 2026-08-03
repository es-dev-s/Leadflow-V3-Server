package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

type HealthPoller struct {
	store      *Store
	crmHealth  string
	interval   time.Duration
	client     *http.Client
	mu         sync.Mutex
	lastStatus string // ok | degraded | offline
}

func NewHealthPoller(store *Store, crmBase string) *HealthPoller {
	base := strings.TrimRight(strings.TrimSpace(crmBase), "/")
	if base == "" {
		base = "http://127.0.0.1:9080"
	}
	return &HealthPoller{
		store:      store,
		crmHealth:  base + "/health",
		interval:   15 * time.Second,
		client:     &http.Client{Timeout: 4 * time.Second},
		lastStatus: "unknown",
	}
}

func (p *HealthPoller) CurrentStatus() string {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.lastStatus == "" || p.lastStatus == "unknown" {
		return "offline"
	}
	return p.lastStatus
}

func (p *HealthPoller) Start(ctx context.Context) {
	go func() {
		p.tick(ctx)
		t := time.NewTicker(p.interval)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.tick(ctx)
			}
		}
	}()
}

func (p *HealthPoller) tick(ctx context.Context) {
	status, msg := p.probe()
	p.mu.Lock()
	prev := p.lastStatus
	p.lastStatus = status
	p.mu.Unlock()

	if prev == status || prev == "unknown" {
		// Still record first observation transition from unknown→something as health_change only when bad.
		if prev == "unknown" && status != "ok" {
			p.recordChange(ctx, prev, status, msg)
		}
		if status == "ok" {
			_ = p.store.CloseOpenIncidents(ctx)
		} else if status == "offline" || status == "degraded" {
			reason := status
			if status == "offline" {
				reason = "unreachable"
			}
			_ = p.store.OpenIncident(ctx, reason)
		}
		return
	}

	p.recordChange(ctx, prev, status, msg)
	if status == "ok" {
		_ = p.store.CloseOpenIncidents(ctx)
	} else {
		reason := status
		if status == "offline" {
			reason = "unreachable"
		}
		_ = p.store.OpenIncident(ctx, reason)
	}
}

func (p *HealthPoller) recordChange(ctx context.Context, from, to, msg string) {
	sev := "info"
	if to == "degraded" {
		sev = "warn"
	}
	if to == "offline" {
		sev = "error"
	}
	code := 0
	ev := IngestEvent{
		Kind:     "health_change",
		Severity: sev,
		Source:   "telemetry",
		Message:  msg,
		Meta:     mustJSON(map[string]any{"from": from, "to": to}),
		Path:     "/health",
		Method:   "GET",
	}
	if to != "ok" {
		code = 503
		ev.StatusCode = &code
	}
	if _, err := p.store.InsertEvents(ctx, []IngestEvent{ev}); err != nil {
		log.Printf("health_change insert: %v", err)
	}
}

func (p *HealthPoller) probe() (status, msg string) {
	res, err := p.client.Get(p.crmHealth)
	if err != nil {
		return "offline", "CRM health unreachable: " + err.Error()
	}
	defer res.Body.Close()
	body, _ := io.ReadAll(io.LimitReader(res.Body, 4096))
	var payload map[string]any
	_ = json.Unmarshal(body, &payload)
	db, _ := payload["database"].(string)
	st, _ := payload["status"].(string)
	if res.StatusCode >= 500 {
		return "offline", "CRM health HTTP " + res.Status
	}
	if strings.EqualFold(st, "degraded") || strings.EqualFold(db, "error") || strings.EqualFold(db, "down") {
		return "degraded", "CRM reported degraded health"
	}
	if strings.EqualFold(st, "ok") || res.StatusCode == 200 {
		return "ok", "CRM healthy"
	}
	return "degraded", "CRM unexpected health payload"
}

func mustJSON(v any) json.RawMessage {
	b, err := json.Marshal(v)
	if err != nil {
		return json.RawMessage(`{}`)
	}
	return b
}
