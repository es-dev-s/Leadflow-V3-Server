// perfcheck measures the hot LeadFlow queries against the live database so we
// can see exactly where multi-million-row latency comes from. Read-only.
package main

import (
	"bufio"
	"context"
	"fmt"
	"log"
	"os"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func envFromFile(path, key string) string {
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func timeQuery(ctx context.Context, pool *pgxpool.Pool, label, sql string, args ...any) {
	start := time.Now()
	rows, err := pool.Query(ctx, sql, args...)
	if err != nil {
		fmt.Printf("%-42s ERROR: %v\n", label, err)
		return
	}
	n := 0
	for rows.Next() {
		n++
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		fmt.Printf("%-42s ERROR: %v\n", label, err)
		return
	}
	fmt.Printf("%-42s %8.0fms  rows=%d\n", label, float64(time.Since(start).Milliseconds()), n)
}

func main() {
	dbURL := envFromFile(".env", "DATABASE_URL")
	if dbURL == "" {
		log.Fatal("DATABASE_URL not found in ./.env")
	}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		log.Fatal(err)
	}
	cfg.MaxConns = 4
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	fmt.Println("=== table stats ===")
	rows, err := pool.Query(ctx, `
		SELECT relname, reltuples::bigint, pg_size_pretty(pg_total_relation_size(oid))
		FROM pg_class
		WHERE relname IN ('Lead','LeadHandoffLog','Notification','LeadView','User','Team')
		  AND relkind = 'r'`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var name, size string
		var tuples int64
		_ = rows.Scan(&name, &tuples, &size)
		fmt.Printf("  %-16s rows≈%-10d size=%s\n", name, tuples, size)
	}
	rows.Close()

	fmt.Println("\n=== indexes ===")
	rows, err = pool.Query(ctx, `
		SELECT tablename, indexname, indexdef
		FROM pg_indexes
		WHERE tablename IN ('Lead','LeadHandoffLog','Notification','LeadView')
		ORDER BY tablename, indexname`)
	if err != nil {
		log.Fatal(err)
	}
	for rows.Next() {
		var t, n, d string
		_ = rows.Scan(&t, &n, &d)
		d = strings.TrimPrefix(d, "CREATE ")
		fmt.Printf("  [%s] %s\n      %s\n", t, n, d)
	}
	rows.Close()

	fmt.Println("\n=== query timings (each run twice: cold-ish, warm) ===")
	const listCols = `l.id, l."leadName", l.phone, l."leadEmail", l.country, l.city, l.source,
		l."portalWebsite", l."clientProfile", l.notes, l."lostNotes",
		l."qualificationStatus", l."leadScore", l."salesStage",
		l."estimatedDealValue", l."closedRevenue", l."initialPayment", l."dealCurrency",
		l."closedAt", l."createdAt", l."updatedAt",
		COALESCE(cb.name, '—'), COALESCE(cb.email, '—'), COALESCE(t.name, '—'), COALESCE(se.name, '—'),
		hh.action, hh.detail`
	const listJoins = `
		FROM "Lead" l
		LEFT JOIN "User" cb ON cb.id = l."createdById"
		LEFT JOIN "Team" t ON t.id = l."teamId"
		LEFT JOIN "User" se ON se.id = l."assignedSalesExecId"
		LEFT JOIN LATERAL (
			SELECT h.action, h.detail
			FROM "LeadHandoffLog" h
			WHERE h."leadId" = l.id AND h.action <> 'LEAD_CREATED'
			ORDER BY h."createdAt" DESC
			LIMIT 1
		) hh ON TRUE`

	queries := []struct {
		label string
		sql   string
	}{
		{"list newest LIMIT 41", `SELECT ` + listCols + listJoins + ` ORDER BY l."createdAt" DESC, l.id DESC LIMIT 41`},
		{"list recent LIMIT 41", `SELECT ` + listCols + listJoins + ` ORDER BY l."updatedAt" DESC, l.id DESC LIMIT 41`},
		{"list name-asc LIMIT 41", `SELECT ` + listCols + listJoins + ` ORDER BY LOWER(l."leadName") ASC, l.id ASC LIMIT 41`},
		{"list newest NO lateral LIMIT 41", `SELECT l.id ` + `
			FROM "Lead" l
			LEFT JOIN "User" cb ON cb.id = l."createdById"
			LEFT JOIN "Team" t ON t.id = l."teamId"
			LEFT JOIN "User" se ON se.id = l."assignedSalesExecId"
			ORDER BY l."createdAt" DESC, l.id DESC LIMIT 41`},
		{"count exact all", `SELECT COUNT(*) FROM "Lead" l`},
		{"count filtered (qualified)", `SELECT COUNT(*) FROM "Lead" l WHERE l."qualificationStatus" = 'QUALIFIED'`},
		{"count filtered (stage no-response)", `SELECT COUNT(*) FROM "Lead" l WHERE l."salesStage" = 'NO_RESPONSE_FROM_CLIENT'`},
		{"summary count-filter aggregate", `
			SELECT
				COUNT(*) FILTER (WHERE l."qualificationStatus" = 'QUALIFIED'),
				COUNT(*) FILTER (WHERE l."qualificationStatus" = 'NOT_QUALIFIED'),
				COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON'),
				COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_LOST'),
				COUNT(*)
			FROM "Lead" l`},
		{"search trgm name", `SELECT l.id FROM "Lead" l WHERE l."leadName" ILIKE '%rahul%' LIMIT 41`},
		{"geo distinct countries", `SELECT DISTINCT l.country FROM "Lead" l WHERE l.country IS NOT NULL LIMIT 300`},
		// Facet filter paths (must hit the BTRIM expression indexes).
		{"facet country page", `SELECT ` + listCols + listJoins + `
			WHERE BTRIM(l.country) = (SELECT BTRIM(country) FROM "Lead" WHERE country IS NOT NULL LIMIT 1)
			ORDER BY l."createdAt" DESC, l.id DESC LIMIT 41`},
		{"facet country count", `SELECT COUNT(*) FROM "Lead" l
			WHERE BTRIM(l.country) = (SELECT BTRIM(country) FROM "Lead" WHERE country IS NOT NULL LIMIT 1)`},
		{"facet source page", `SELECT ` + listCols + listJoins + `
			WHERE BTRIM(l.source) = (SELECT BTRIM(source) FROM "Lead" WHERE source IS NOT NULL LIMIT 1)
			ORDER BY l."createdAt" DESC, l.id DESC LIMIT 41`},
		{"facet source count", `SELECT COUNT(*) FROM "Lead" l
			WHERE BTRIM(l.source) = (SELECT BTRIM(source) FROM "Lead" WHERE source IS NOT NULL LIMIT 1)`},
		{"facet city page", `SELECT ` + listCols + listJoins + `
			WHERE BTRIM(l.city) = (SELECT BTRIM(city) FROM "Lead" WHERE city IS NOT NULL LIMIT 1)
			ORDER BY l."createdAt" DESC, l.id DESC LIMIT 41`},
		{"facet portal count", `SELECT COUNT(*) FROM "Lead" l
			WHERE BTRIM(l."portalWebsite") = (SELECT BTRIM("portalWebsite") FROM "Lead" WHERE "portalWebsite" IS NOT NULL LIMIT 1)`},
	}

	for _, q := range queries {
		for i := 0; i < 2; i++ {
			timeQuery(ctx, pool, fmt.Sprintf("%s (run %d)", q.label, i+1), q.sql)
		}
	}
}
