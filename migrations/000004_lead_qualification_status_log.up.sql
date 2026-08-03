-- Append-only log of qualification status changes for duration analytics.
CREATE TABLE IF NOT EXISTS "LeadQualificationStatusLog" (
	id TEXT PRIMARY KEY,
	"leadId" TEXT NOT NULL REFERENCES "Lead"(id) ON DELETE CASCADE,
	"fromStatus" TEXT,
	"toStatus" TEXT NOT NULL,
	"changedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW(),
	"actorId" TEXT REFERENCES "User"(id) ON DELETE SET NULL,
	reason TEXT,
	source TEXT NOT NULL DEFAULT 'update'
);

CREATE INDEX IF NOT EXISTS "LeadQualificationStatusLog_lead_changed_idx"
	ON "LeadQualificationStatusLog" ("leadId", "changedAt" ASC);

ALTER TABLE "Lead"
	ADD COLUMN IF NOT EXISTS "qualificationEnteredAt" TIMESTAMPTZ;
