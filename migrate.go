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
var crmMigrationsFS embed.FS

// RunCRMMigrations applies versioned SQL under migrations/ exactly once each.
// Statements are idempotent (IF NOT EXISTS) so existing production DBs that
// were previously mutated at startup remain safe.
func RunCRMMigrations(ctx context.Context, pool *pgxpool.Pool) error {
	if _, err := pool.Exec(ctx, `
		CREATE TABLE IF NOT EXISTS schema_migrations (
			version TEXT PRIMARY KEY,
			applied_at TIMESTAMPTZ NOT NULL DEFAULT NOW()
		)`); err != nil {
		return fmt.Errorf("create schema_migrations: %w", err)
	}

	entries, err := fs.ReadDir(crmMigrationsFS, "migrations")
	if err != nil {
		return fmt.Errorf("read migrations: %w", err)
	}
	names := make([]string, 0, len(entries))
	for _, e := range entries {
		if e.IsDir() {
			continue
		}
		name := e.Name()
		if !strings.HasSuffix(name, ".up.sql") {
			continue
		}
		names = append(names, name)
	}
	sort.Strings(names)

	for _, name := range names {
		version := strings.TrimSuffix(name, ".up.sql")
		var exists bool
		if err := pool.QueryRow(ctx,
			`SELECT EXISTS (SELECT 1 FROM schema_migrations WHERE version = $1)`, version,
		).Scan(&exists); err != nil {
			return fmt.Errorf("check migration %s: %w", version, err)
		}
		if exists {
			continue
		}

		body, err := crmMigrationsFS.ReadFile(path.Join("migrations", name))
		if err != nil {
			return fmt.Errorf("read migration %s: %w", name, err)
		}
		log.Printf("migrate: applying %s", version)
		if err := execMigrationSQL(ctx, pool, string(body)); err != nil {
			return fmt.Errorf("apply %s: %w", version, err)
		}
		if _, err := pool.Exec(ctx,
			`INSERT INTO schema_migrations (version) VALUES ($1)`, version,
		); err != nil {
			return fmt.Errorf("record %s: %w", version, err)
		}
		log.Printf("migrate: applied %s", version)
	}
	return nil
}

func execMigrationSQL(ctx context.Context, pool *pgxpool.Pool, body string) error {
	for _, stmt := range splitSQLStatements(body) {
		if _, err := pool.Exec(ctx, stmt); err != nil {
			lower := strings.ToLower(stmt)
			// Managed Postgres may restrict CREATE EXTENSION; trigram indexes
			// then also fail. Soft-fail those so the rest of the migration applies.
			if strings.Contains(lower, "pg_trgm") || strings.Contains(lower, "gin_trgm_ops") {
				log.Printf("migrate: soft-fail statement: %v", err)
				continue
			}
			return err
		}
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
