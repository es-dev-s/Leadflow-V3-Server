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
