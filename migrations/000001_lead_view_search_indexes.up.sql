-- LeadView tracking + search/sort/facet indexes + first-response / not-appropriate columns.
-- Idempotent: safe on fresh DBs and on databases previously provisioned at runtime.

CREATE TABLE IF NOT EXISTS "LeadView" (
	"userId" TEXT NOT NULL REFERENCES "User"(id) ON DELETE CASCADE,
	"leadId" TEXT NOT NULL REFERENCES "Lead"(id) ON DELETE CASCADE,
	"viewedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	PRIMARY KEY ("userId", "leadId")
);

CREATE INDEX IF NOT EXISTS "LeadView_leadId_idx" ON "LeadView" ("leadId");

CREATE EXTENSION IF NOT EXISTS pg_trgm;

CREATE INDEX IF NOT EXISTS "Lead_leadName_trgm_idx"
	ON "Lead" USING gin ("leadName" gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "Lead_phone_trgm_idx"
	ON "Lead" USING gin (phone gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "User_name_trgm_idx"
	ON "User" USING gin (name gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "Lead_leadName_lower_id_idx"
	ON "Lead" (LOWER("leadName") ASC, id ASC);

CREATE INDEX IF NOT EXISTS "Lead_estimatedDealValue_sort_idx"
	ON "Lead" ("estimatedDealValue" DESC NULLS LAST, "createdAt" DESC, id DESC);

CREATE INDEX IF NOT EXISTS "Lead_updatedAt_id_desc_idx"
	ON "Lead" ("updatedAt" DESC, id DESC);

CREATE INDEX IF NOT EXISTS "Lead_country_btrim_created_idx"
	ON "Lead" (BTRIM(country), "createdAt" DESC, id DESC);

CREATE INDEX IF NOT EXISTS "Lead_city_btrim_created_idx"
	ON "Lead" (BTRIM(city), "createdAt" DESC, id DESC);

CREATE INDEX IF NOT EXISTS "Lead_source_btrim_created_idx"
	ON "Lead" (BTRIM(source), "createdAt" DESC, id DESC);

CREATE INDEX IF NOT EXISTS "Lead_portalWebsite_btrim_created_idx"
	ON "Lead" (BTRIM("portalWebsite"), "createdAt" DESC, id DESC);

CREATE INDEX IF NOT EXISTS "Lead_metaProfile_btrim_created_idx"
	ON "Lead" (BTRIM("sourceMetaProfileName"), "createdAt" DESC, id DESC);

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
) STORED;

CREATE INDEX IF NOT EXISTS "Lead_searchDoc_trgm_idx"
	ON "Lead" USING gin ("searchDoc" gin_trgm_ops);

CREATE INDEX IF NOT EXISTS "Lead_leadEmail_lower_idx"
	ON "Lead" (lower("leadEmail"))
	WHERE "leadEmail" IS NOT NULL AND BTRIM("leadEmail") <> '';

CREATE INDEX IF NOT EXISTS "Lead_phone_digits_idx"
	ON "Lead" (regexp_replace(COALESCE(phone, ''), '[^0-9]', '', 'g'))
	WHERE phone IS NOT NULL AND BTRIM(phone) <> '';

ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "firstClientMessageAt" timestamptz;
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "firstAgentMessageAt" timestamptz;
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "firstResponseMinutes" integer;
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "firstResponseProofPath" text;
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "notAppropriate" boolean NOT NULL DEFAULT FALSE;
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "notAppropriateReason" text;
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "notAppropriateAt" timestamptz;
ALTER TABLE "Lead" ADD COLUMN IF NOT EXISTS "notAppropriateById" text;

CREATE INDEX IF NOT EXISTS "Lead_notAppropriate_idx"
	ON "Lead" ("notAppropriate")
	WHERE "notAppropriate" = TRUE;
