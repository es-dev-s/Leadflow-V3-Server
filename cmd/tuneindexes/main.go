// tuneindexes adds the sort indexes the leads list needs at multi-million-row
// scale. Uses CREATE INDEX CONCURRENTLY so production traffic is not blocked.
// Safe to re-run: every statement is IF NOT EXISTS.
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

var statements = []string{
	// "Name A–Z" sort: ORDER BY LOWER("leadName"), id — measured ~2.1s without
	// this index on 1M rows, single-digit ms with it.
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_leadName_lower_id_idx"
		ON "Lead" (LOWER("leadName") ASC, id ASC)`,
	// "Deal value" sort: ORDER BY "estimatedDealValue" DESC NULLS LAST, "createdAt" DESC, id DESC.
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_estimatedDealValue_sort_idx"
		ON "Lead" ("estimatedDealValue" DESC NULLS LAST, "createdAt" DESC, id DESC)`,
	// "Recent" sort: ORDER BY "updatedAt" DESC, id DESC.
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_updatedAt_id_desc_idx"
		ON "Lead" ("updatedAt" DESC, id DESC)`,
	// Facet filters compare BTRIM(col) (see appendLeadFacets), so plain column
	// indexes never match. Composite with (createdAt DESC, id DESC) serves the
	// default "newest" sort directly from the index — no sort node even when a
	// filter matches hundreds of thousands of rows.
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_country_btrim_created_idx"
		ON "Lead" (BTRIM(country), "createdAt" DESC, id DESC)`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_city_btrim_created_idx"
		ON "Lead" (BTRIM(city), "createdAt" DESC, id DESC)`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_source_btrim_created_idx"
		ON "Lead" (BTRIM(source), "createdAt" DESC, id DESC)`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_portalWebsite_btrim_created_idx"
		ON "Lead" (BTRIM("portalWebsite"), "createdAt" DESC, id DESC)`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_metaProfile_btrim_created_idx"
		ON "Lead" (BTRIM("sourceMetaProfileName"), "createdAt" DESC, id DESC)`,
	// All-fields search document: one lower-cased text column covering every
	// lead field the global search box targets, so search is a single indexed
	// LIKE instead of an unindexable 18-column OR. The ALTER rewrites the
	// table (takes an exclusive lock for the duration — run off-peak).
	`ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "searchDoc" text
	GENERATED ALWAYS AS (
		lower(
			coalesce("leadName", '') || ' ' ||
			coalesce("leadEmail", '') || ' ' ||
			coalesce(phone, '') || ' ' ||
			translate(coalesce(phone, ''), ' ()-+.', '') || ' ' ||
			coalesce(source, '') || ' ' ||
			coalesce("portalWebsite", '') || ' ' ||
			coalesce("sourceMetaProfileName", '') || ' ' ||
			coalesce("clientProfile", '') || ' ' ||
			coalesce(city, '') || ' ' ||
			coalesce(country, '') || ' ' ||
			coalesce(notes, '') || ' ' ||
			coalesce("lostNotes", '') || ' ' ||
			coalesce("qualificationStatus", '') || ' ' ||
			replace(coalesce("qualificationStatus", ''), '_', ' ') || ' ' ||
			coalesce("salesStage", '') || ' ' ||
			replace(coalesce("salesStage", ''), '_', ' ') || ' ' ||
			coalesce("dealCurrency", '')
		)
	) STORED`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_searchDoc_trgm_idx"
		ON "Lead" USING gin ("searchDoc" gin_trgm_ops)`,
	// Duplicate contact checks on create/update.
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_leadEmail_lower_idx"
		ON "Lead" (lower("leadEmail"))
		WHERE "leadEmail" IS NOT NULL AND BTRIM("leadEmail") <> ''`,
	`CREATE INDEX CONCURRENTLY IF NOT EXISTS "Lead_phone_digits_idx"
		ON "Lead" (regexp_replace(COALESCE(phone, ''), '[^0-9]', '', 'g'))
		WHERE phone IS NOT NULL AND BTRIM(phone) <> ''`,
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
	cfg.MaxConns = 2
	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatal(err)
	}
	defer pool.Close()

	for _, stmt := range statements {
		name := "index"
		if i := strings.Index(stmt, `"Lead_`); i >= 0 {
			if j := strings.Index(stmt[i+1:], `"`); j >= 0 {
				name = stmt[i+1 : i+1+j]
			}
		}
		start := time.Now()
		fmt.Printf("creating %s …\n", name)
		if _, err := pool.Exec(ctx, stmt); err != nil {
			log.Fatalf("%s: %v", name, err)
		}
		fmt.Printf("  done in %s\n", time.Since(start).Round(time.Millisecond))
	}
	fmt.Println("all indexes ready")
}
