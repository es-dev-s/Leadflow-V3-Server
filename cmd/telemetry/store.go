package main

import (
	"context"
	"encoding/json"
	"fmt"
	"log"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

type Store struct {
	pool *pgxpool.Pool
}

type TelemetryEvent struct {
	ID          string          `json:"id"`
	OccurredAt  time.Time       `json:"occurredAt"`
	Kind        string          `json:"kind"`
	Severity    string          `json:"severity"`
	Source      string          `json:"source"`
	StatusCode  *int            `json:"statusCode,omitempty"`
	Path        string          `json:"path,omitempty"`
	Method      string          `json:"method,omitempty"`
	UserID      string          `json:"userId,omitempty"`
	UserEmail   string          `json:"userEmail,omitempty"`
	Message     string          `json:"message,omitempty"`
	Meta        json.RawMessage `json:"meta,omitempty"`
}

type IngestEvent struct {
	OccurredAt *time.Time      `json:"occurredAt"`
	Kind       string          `json:"kind"`
	Severity   string          `json:"severity"`
	Source     string          `json:"source"`
	StatusCode *int            `json:"statusCode"`
	Path       string          `json:"path"`
	Method     string          `json:"method"`
	UserID     string          `json:"userId"`
	UserEmail  string          `json:"userEmail"`
	Message    string          `json:"message"`
	Meta       json.RawMessage `json:"meta"`
}

type UptimeIncident struct {
	ID        string     `json:"id"`
	StartedAt time.Time  `json:"startedAt"`
	EndedAt   *time.Time `json:"endedAt,omitempty"`
	Reason    string     `json:"reason"`
	Open      bool       `json:"open"`
}

type StatusBucket struct {
	Hour       time.Time `json:"hour"`
	StatusCode int       `json:"statusCode"`
	Count      int64     `json:"count"`
}

type KindBucket struct {
	Hour  time.Time `json:"hour"`
	Kind  string    `json:"kind"`
	Count int64     `json:"count"`
}

type StatusTotal struct {
	StatusCode int   `json:"statusCode"`
	Count      int64 `json:"count"`
}

type Overview struct {
	GeneratedAt          time.Time         `json:"generatedAt"`
	ActiveUsers          int64             `json:"activeUsers"`
	ConcurrentUsers      int64             `json:"concurrentUsers"`
	PlatformStatus       string            `json:"platformStatus"`
	OpenDowntimeMinutes  float64           `json:"openDowntimeMinutes"`
	DowntimeEvents24h    int64             `json:"downtimeEvents24h"`
	ConnectionBreaks24h  int64             `json:"connectionBreaks24h"`
	ServerRestarts24h    int64             `json:"serverRestarts24h"`
	HTTPErrors24h        int64             `json:"httpErrors24h"`
	StatusTotals24h      []StatusTotal     `json:"statusTotals24h"`
	StatusSeries24h      []StatusBucket    `json:"statusSeries24h"`
	ConnectionSeries24h  []KindBucket      `json:"connectionSeries24h"`
	Incidents            []UptimeIncident  `json:"incidents"`
	RecentEvents         []TelemetryEvent  `json:"recentEvents"`
}

func NewStore(pool *pgxpool.Pool) *Store {
	return &Store{pool: pool}
}

func (s *Store) EnsureSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
CREATE SCHEMA IF NOT EXISTS support_telemetry;

CREATE TABLE IF NOT EXISTS support_telemetry.telemetry_event (
  id           text PRIMARY KEY,
  occurred_at  timestamptz NOT NULL DEFAULT NOW(),
  kind         text NOT NULL,
  severity     text NOT NULL DEFAULT 'info',
  source       text NOT NULL DEFAULT 'unknown',
  status_code  int,
  path         text,
  method       text,
  user_id      text,
  user_email   text,
  message      text,
  meta         jsonb NOT NULL DEFAULT '{}'::jsonb
);

CREATE INDEX IF NOT EXISTS telemetry_event_occurred_idx
  ON support_telemetry.telemetry_event (occurred_at DESC);
CREATE INDEX IF NOT EXISTS telemetry_event_kind_occurred_idx
  ON support_telemetry.telemetry_event (kind, occurred_at DESC);
CREATE INDEX IF NOT EXISTS telemetry_event_status_occurred_idx
  ON support_telemetry.telemetry_event (status_code, occurred_at DESC)
  WHERE status_code IS NOT NULL;

CREATE TABLE IF NOT EXISTS support_telemetry.uptime_incident (
  id          text PRIMARY KEY,
  started_at  timestamptz NOT NULL,
  ended_at    timestamptz,
  reason      text NOT NULL
);

CREATE INDEX IF NOT EXISTS uptime_incident_started_idx
  ON support_telemetry.uptime_incident (started_at DESC);
CREATE INDEX IF NOT EXISTS uptime_incident_open_idx
  ON support_telemetry.uptime_incident (started_at DESC)
  WHERE ended_at IS NULL;
`)
	return err
}

func (s *Store) InsertEvents(ctx context.Context, events []IngestEvent) (int, error) {
	if len(events) == 0 {
		return 0, nil
	}
	n := 0
	for _, e := range events {
		kind := strings.TrimSpace(e.Kind)
		if kind == "" {
			continue
		}
		severity := strings.TrimSpace(e.Severity)
		if severity == "" {
			severity = "info"
		}
		source := strings.TrimSpace(e.Source)
		if source == "" {
			source = "unknown"
		}
		meta := e.Meta
		if len(meta) == 0 {
			meta = json.RawMessage(`{}`)
		}
		occurred := nowUTC()
		if e.OccurredAt != nil && !e.OccurredAt.IsZero() {
			occurred = e.OccurredAt.UTC()
		}
		id := uuid.NewString()
		_, err := s.pool.Exec(ctx, `
INSERT INTO support_telemetry.telemetry_event
  (id, occurred_at, kind, severity, source, status_code, path, method, user_id, user_email, message, meta)
VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12)
`, id, occurred, kind, severity, source, e.StatusCode,
			nullIfEmpty(e.Path), nullIfEmpty(e.Method), nullIfEmpty(e.UserID),
			nullIfEmpty(e.UserEmail), nullIfEmpty(e.Message), meta)
		if err != nil {
			return n, err
		}
		n++
	}
	return n, nil
}

func nullIfEmpty(s string) any {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return s
}

func (s *Store) OpenIncident(ctx context.Context, reason string) error {
	var openID string
	err := s.pool.QueryRow(ctx, `
SELECT id FROM support_telemetry.uptime_incident
WHERE ended_at IS NULL
ORDER BY started_at DESC LIMIT 1`).Scan(&openID)
	if err == nil && openID != "" {
		return nil // already open
	}
	_, err = s.pool.Exec(ctx, `
INSERT INTO support_telemetry.uptime_incident (id, started_at, reason)
VALUES ($1, NOW(), $2)`, uuid.NewString(), reason)
	return err
}

func (s *Store) CloseOpenIncidents(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
UPDATE support_telemetry.uptime_incident
SET ended_at = NOW()
WHERE ended_at IS NULL`)
	return err
}

func (s *Store) LastServerStart(ctx context.Context) (*time.Time, error) {
	var t time.Time
	err := s.pool.QueryRow(ctx, `
SELECT occurred_at FROM support_telemetry.telemetry_event
WHERE kind = 'server_start' AND source = 'crm'
ORDER BY occurred_at DESC LIMIT 1`).Scan(&t)
	if err != nil {
		return nil, err
	}
	return &t, nil
}

func (s *Store) Overview(ctx context.Context, activeUsers, concurrent int64, platformStatus string) (*Overview, error) {
	out := &Overview{
		GeneratedAt:     nowUTC(),
		ActiveUsers:     activeUsers,
		ConcurrentUsers: concurrent,
		PlatformStatus:  platformStatus,
		StatusTotals24h: []StatusTotal{},
		StatusSeries24h: []StatusBucket{},
		ConnectionSeries24h: []KindBucket{},
		Incidents:       []UptimeIncident{},
		RecentEvents:    []TelemetryEvent{},
	}

	since := nowUTC().Add(-24 * time.Hour)

	_ = s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM support_telemetry.telemetry_event
WHERE kind IN ('health_change','server_stop') AND occurred_at >= $1
  AND (message ILIKE '%offline%' OR message ILIKE '%unreachable%' OR kind = 'server_stop')`, since).Scan(&out.DowntimeEvents24h)

	_ = s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM support_telemetry.telemetry_event
WHERE kind = 'connection_break' AND occurred_at >= $1`, since).Scan(&out.ConnectionBreaks24h)

	_ = s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM support_telemetry.telemetry_event
WHERE kind = 'server_start' AND source = 'crm' AND occurred_at >= $1`, since).Scan(&out.ServerRestarts24h)

	_ = s.pool.QueryRow(ctx, `
SELECT COUNT(*) FROM support_telemetry.telemetry_event
WHERE kind IN ('http_status','client_error') AND occurred_at >= $1
  AND status_code IS NOT NULL AND status_code >= 400`, since).Scan(&out.HTTPErrors24h)

	var openStart *time.Time
	_ = s.pool.QueryRow(ctx, `
SELECT started_at FROM support_telemetry.uptime_incident
WHERE ended_at IS NULL ORDER BY started_at DESC LIMIT 1`).Scan(&openStart)
	if openStart != nil {
		out.OpenDowntimeMinutes = nowUTC().Sub(*openStart).Minutes()
	}

	rows, err := s.pool.Query(ctx, `
SELECT COALESCE(status_code, 0), COUNT(*)
FROM support_telemetry.telemetry_event
WHERE occurred_at >= $1 AND status_code IS NOT NULL AND status_code >= 400
GROUP BY status_code ORDER BY COUNT(*) DESC`, since)
	if err == nil {
		defer rows.Close()
		for rows.Next() {
			var st StatusTotal
			if err := rows.Scan(&st.StatusCode, &st.Count); err == nil {
				out.StatusTotals24h = append(out.StatusTotals24h, st)
			}
		}
	}

	rows2, err := s.pool.Query(ctx, `
SELECT date_trunc('hour', occurred_at) AS hour, status_code, COUNT(*)
FROM support_telemetry.telemetry_event
WHERE occurred_at >= $1 AND status_code IS NOT NULL AND status_code >= 400
GROUP BY 1, 2 ORDER BY 1 ASC`, since)
	if err == nil {
		defer rows2.Close()
		for rows2.Next() {
			var b StatusBucket
			if err := rows2.Scan(&b.Hour, &b.StatusCode, &b.Count); err == nil {
				out.StatusSeries24h = append(out.StatusSeries24h, b)
			}
		}
	}

	rows3, err := s.pool.Query(ctx, `
SELECT date_trunc('hour', occurred_at) AS hour, kind, COUNT(*)
FROM support_telemetry.telemetry_event
WHERE occurred_at >= $1 AND kind IN ('connection_break','server_start','server_stop','health_change')
GROUP BY 1, 2 ORDER BY 1 ASC`, since)
	if err == nil {
		defer rows3.Close()
		for rows3.Next() {
			var b KindBucket
			if err := rows3.Scan(&b.Hour, &b.Kind, &b.Count); err == nil {
				out.ConnectionSeries24h = append(out.ConnectionSeries24h, b)
			}
		}
	}

	incRows, err := s.pool.Query(ctx, `
SELECT id, started_at, ended_at, reason
FROM support_telemetry.uptime_incident
ORDER BY started_at DESC LIMIT 20`)
	if err == nil {
		defer incRows.Close()
		for incRows.Next() {
			var inc UptimeIncident
			var ended *time.Time
			if err := incRows.Scan(&inc.ID, &inc.StartedAt, &ended, &inc.Reason); err == nil {
				inc.EndedAt = ended
				inc.Open = ended == nil
				out.Incidents = append(out.Incidents, inc)
			}
		}
	}

	evRows, err := s.pool.Query(ctx, `
SELECT id, occurred_at, kind, severity, source, status_code, COALESCE(path,''), COALESCE(method,''),
       COALESCE(user_id,''), COALESCE(user_email,''), COALESCE(message,''), meta
FROM support_telemetry.telemetry_event
ORDER BY occurred_at DESC LIMIT 50`)
	if err == nil {
		defer evRows.Close()
		for evRows.Next() {
			var ev TelemetryEvent
			var meta []byte
			if err := evRows.Scan(&ev.ID, &ev.OccurredAt, &ev.Kind, &ev.Severity, &ev.Source,
				&ev.StatusCode, &ev.Path, &ev.Method, &ev.UserID, &ev.UserEmail, &ev.Message, &meta); err == nil {
				if len(meta) > 0 {
					ev.Meta = meta
				}
				out.RecentEvents = append(out.RecentEvents, ev)
			}
		}
	}

	return out, nil
}

func (s *Store) ListEvents(ctx context.Context, limit int, before *time.Time) ([]TelemetryEvent, error) {
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	var rows pgxRows
	var err error
	if before != nil {
		rows, err = s.pool.Query(ctx, `
SELECT id, occurred_at, kind, severity, source, status_code, COALESCE(path,''), COALESCE(method,''),
       COALESCE(user_id,''), COALESCE(user_email,''), COALESCE(message,''), meta
FROM support_telemetry.telemetry_event
WHERE occurred_at < $1
ORDER BY occurred_at DESC LIMIT $2`, *before, limit)
	} else {
		rows, err = s.pool.Query(ctx, `
SELECT id, occurred_at, kind, severity, source, status_code, COALESCE(path,''), COALESCE(method,''),
       COALESCE(user_id,''), COALESCE(user_email,''), COALESCE(message,''), meta
FROM support_telemetry.telemetry_event
ORDER BY occurred_at DESC LIMIT $1`, limit)
	}
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TelemetryEvent, 0, limit)
	for rows.Next() {
		var ev TelemetryEvent
		var meta []byte
		if err := rows.Scan(&ev.ID, &ev.OccurredAt, &ev.Kind, &ev.Severity, &ev.Source,
			&ev.StatusCode, &ev.Path, &ev.Method, &ev.UserID, &ev.UserEmail, &ev.Message, &meta); err != nil {
			return out, err
		}
		if len(meta) > 0 {
			ev.Meta = meta
		}
		out = append(out, ev)
	}
	return out, rows.Err()
}

// pgxRows narrow interface for Query results
type pgxRows interface {
	Next() bool
	Scan(dest ...any) error
	Close()
	Err() error
}

func deriveTelemetryDatabaseURL(crmURL string) string {
	// Prefer dedicated DB name when TELEMETRY_DATABASE_URL is unset.
	// Falls back to same URL with schema (EnsureSchema uses support_telemetry).
	if crmURL == "" {
		return ""
	}
	// If already pointing at leadflow_telemetry, keep it.
	if strings.Contains(crmURL, "/leadflow_telemetry") {
		return crmURL
	}
	// Try swap final path segment database name.
	idx := strings.LastIndex(crmURL, "/")
	if idx < 0 {
		return crmURL
	}
	base := crmURL[:idx+1]
	rest := crmURL[idx+1:]
	// strip query
	q := ""
	if qi := strings.Index(rest, "?"); qi >= 0 {
		q = rest[qi:]
	}
	return base + "leadflow_telemetry" + q
}

func connectStore(ctx context.Context, preferred, fallback string) (*pgxpool.Pool, string, error) {
	tryURLs := []string{}
	if preferred != "" {
		tryURLs = append(tryURLs, preferred)
	}
	if fallback != "" && fallback != preferred {
		tryURLs = append(tryURLs, fallback)
	}
	var lastErr error
	for _, u := range tryURLs {
		cfg, err := pgxpool.ParseConfig(u)
		if err != nil {
			lastErr = err
			continue
		}
		cfg.MaxConns = 16
		cfg.MinConns = 2
		cfg.MaxConnLifetime = 30 * time.Minute
		cfg.MaxConnIdleTime = 5 * time.Minute
		cfg.ConnConfig.ConnectTimeout = 5 * time.Second
		pool, err := pgxpool.NewWithConfig(ctx, cfg)
		if err != nil {
			lastErr = err
			continue
		}
		pingCtx, cancel := context.WithTimeout(ctx, 5*time.Second)
		err = pool.Ping(pingCtx)
		cancel()
		if err != nil {
			pool.Close()
			lastErr = err
			log.Printf("telemetry db ping failed for %s: %v", redactURL(u), err)
			continue
		}
		return pool, u, nil
	}
	if lastErr == nil {
		lastErr = fmt.Errorf("no database URL configured")
	}
	return nil, "", lastErr
}

func redactURL(u string) string {
	if i := strings.Index(u, "@"); i > 0 {
		return "***@" + u[i+1:]
	}
	return u
}
