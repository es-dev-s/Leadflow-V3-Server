package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// leadSearchDocSQL is the ALTER statement for the precomputed all-fields
// search document. Every expression must be IMMUTABLE (generated column
// requirement); phone digits are embedded so formatted numbers still match.
const leadSearchDocSQL = `
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "searchDoc" text
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
) STORED`

const ensureLeadViewSQL = `
CREATE TABLE IF NOT EXISTS "LeadView" (
	"userId" TEXT NOT NULL REFERENCES "User"(id) ON DELETE CASCADE,
	"leadId" TEXT NOT NULL REFERENCES "Lead"(id) ON DELETE CASCADE,
	"viewedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY ("userId", "leadId")
);
CREATE INDEX IF NOT EXISTS "LeadView_leadId_idx" ON "LeadView" ("leadId");
`

func (s *LeadStore) EnsureLeadViewSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, ensureLeadViewSQL)
	if err != nil {
		return err
	}
	// Best-effort trigram indexes for production contains-search. Soft-fail if
	// the extension isn't available (managed DBs often restrict CREATE EXTENSION).
	_, _ = s.pool.Exec(ctx, `CREATE EXTENSION IF NOT EXISTS pg_trgm`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_leadName_trgm_idx"
		ON "Lead" USING gin ("leadName" gin_trgm_ops)`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_phone_trgm_idx"
		ON "Lead" USING gin (phone gin_trgm_ops)`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "User_name_trgm_idx"
		ON "User" USING gin (name gin_trgm_ops)`)
	// Sort indexes for the leads list at multi-million-row scale. Without the
	// LOWER(leadName) index the A–Z sort is a full-table sort (~2s on 1M rows).
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_leadName_lower_id_idx"
		ON "Lead" (LOWER("leadName") ASC, id ASC)`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_estimatedDealValue_sort_idx"
		ON "Lead" ("estimatedDealValue" DESC NULLS LAST, "createdAt" DESC, id DESC)`)
	// "Recent" sort: ORDER BY "updatedAt" DESC, id DESC — every edit/assign/
	// rename/qualify bumps updatedAt, so this surfaces live activity.
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_updatedAt_id_desc_idx"
		ON "Lead" ("updatedAt" DESC, id DESC)`)
	// Facet filter indexes: appendLeadFacets compares BTRIM(col), so these must
	// be expression indexes. The (createdAt DESC, id DESC) suffix serves the
	// default "newest" sort straight from the index.
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_country_btrim_created_idx"
		ON "Lead" (BTRIM(country), "createdAt" DESC, id DESC)`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_city_btrim_created_idx"
		ON "Lead" (BTRIM(city), "createdAt" DESC, id DESC)`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_source_btrim_created_idx"
		ON "Lead" (BTRIM(source), "createdAt" DESC, id DESC)`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_portalWebsite_btrim_created_idx"
		ON "Lead" (BTRIM("portalWebsite"), "createdAt" DESC, id DESC)`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_metaProfile_btrim_created_idx"
		ON "Lead" (BTRIM("sourceMetaProfileName"), "createdAt" DESC, id DESC)`)
	// All-fields search document + index. On a large existing table run
	// cmd/tuneindexes first (this best-effort boot path may exceed its
	// timeout); on fresh databases these complete instantly.
	_, _ = s.pool.Exec(ctx, leadSearchDocSQL)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_searchDoc_trgm_idx"
		ON "Lead" USING gin ("searchDoc" gin_trgm_ops)`)
	// Duplicate-contact lookups used on every create/update.
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_leadEmail_lower_idx"
		ON "Lead" (lower("leadEmail"))
		WHERE "leadEmail" IS NOT NULL AND BTRIM("leadEmail") <> ''`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_phone_digits_idx"
		ON "Lead" (regexp_replace(COALESCE(phone, ''), '[^0-9]', '', 'g'))
		WHERE phone IS NOT NULL AND BTRIM(phone) <> ''`)
	// First-response SLA fields (client/agent message times + derived minutes + proof).
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "firstClientMessageAt" timestamptz`)
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "firstAgentMessageAt" timestamptz`)
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "firstResponseMinutes" integer`)
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "firstResponseProofPath" text`)
	// SE / staff flag: lead is not appropriate for sales (with required reason).
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "notAppropriate" boolean NOT NULL DEFAULT FALSE`)
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "notAppropriateReason" text`)
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "notAppropriateAt" timestamptz`)
	_, _ = s.pool.Exec(ctx, `
		ALTER TABLE "Lead"
		ADD COLUMN IF NOT EXISTS "notAppropriateById" text`)
	_, _ = s.pool.Exec(ctx, `
		CREATE INDEX IF NOT EXISTS "Lead_notAppropriate_idx"
		ON "Lead" ("notAppropriate")
		WHERE "notAppropriate" = TRUE`)
	return nil
}

func (s *LeadStore) MarkLeadViewed(ctx context.Context, userID, leadID string) error {
	userID = strings.TrimSpace(userID)
	leadID = strings.TrimSpace(leadID)
	if userID == "" || leadID == "" {
		return fmt.Errorf("user and lead are required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO "LeadView" ("userId", "leadId", "viewedAt")
		VALUES ($1, $2, $3)
		ON CONFLICT ("userId", "leadId")
		DO UPDATE SET "viewedAt" = EXCLUDED."viewedAt"`,
		userID, leadID, time.Now().UTC(),
	)
	return err
}
