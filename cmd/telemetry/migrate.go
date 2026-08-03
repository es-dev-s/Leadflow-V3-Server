package main

import (
	"context"
	"embed"
	"fmt"
	"io/fs"
	"log"
	"path"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgxpool"
)

//go:embed migrations/*.sql
var telemetryMigrationsFS embed.FS

func RunTelemetryMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS support_telemetry_schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create support_telemetry_schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(telemetryMigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() || !strings.HasSuffix(e.Name(), ".up.sql") {
			continue
		}
		names = append(names, e.Name())
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".up.sql")
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM support_telemetry_schema_migrations WHERE version = $1)`,
			version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists {
			continue
		}
		body, err := telemetryMigrationsFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read %s: %w", name, err)
		}
		log.Printf("telemetry migrate: applying %s", version)
		for _, stmt := range splitSQLStatements(string(body)) {
			if _, err := pool.Exec(ctx, stmt); err != nil {
				return fmt.Errorf("apply %s: %w", version, err)
			}
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO support_telemetry_schema_migrations (version) VALUES ($1)`, version,
		); err != nil {
			return fmt.Errorf("record %s: %w", version, err)
		}
		log.Printf("telemetry migrate: applied %s", version)
	}
	return nil
}

func splitSQLStatements(body string) []string {
	lines := strings.Split(body, "\n")
	var b strings.Builder
	var out []string
	for _, line := range lines {
		trim := strings.TrimSpace(line)
		if trim == "" || strings.HasPrefix(trim, "--") {
			continue
		}
		b.WriteString(line)
		b.WriteByte('\n')
		if strings.HasSuffix(trim, ";") {
			stmt := strings.TrimSpace(b.String())
			stmt = strings.TrimSuffix(stmt, ";")
			stmt = strings.TrimSpace(stmt)
			if stmt != "" {
				out = append(out, stmt)
			}
			b.Reset()
		}
	}
	if rest := strings.TrimSpace(b.String()); rest != "" {
		out = append(out, strings.TrimSuffix(rest, ";"))
	}
	return out
}
