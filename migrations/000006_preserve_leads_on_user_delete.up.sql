-- Leads belong to the CRM, not to a user account.
-- Deleting a Sales Executive (or any user) must never cascade-delete Lead rows.
-- Assignment FKs already SET NULL; createdById was CASCADE and notAppropriateById
-- had no FK (or CASCADE in some Prisma-era databases).

-- Creator: keep the lead, drop the pointer if the user row is removed.
ALTER TABLE "Lead" DROP CONSTRAINT IF EXISTS "Lead_createdById_fkey";
ALTER TABLE "Lead" ALTER COLUMN "createdById" DROP NOT NULL;
ALTER TABLE "Lead" ADD CONSTRAINT "Lead_createdById_fkey"
	FOREIGN KEY ("createdById") REFERENCES "User"(id)
	ON UPDATE CASCADE ON DELETE SET NULL;

-- Who marked not-appropriate / Irrelevant: same rule.
UPDATE "Lead" l
SET "notAppropriateById" = NULL
WHERE l."notAppropriateById" IS NOT NULL
  AND NOT EXISTS (SELECT 1 FROM "User" u WHERE u.id = l."notAppropriateById");

ALTER TABLE "Lead" DROP CONSTRAINT IF EXISTS "Lead_notAppropriateById_fkey";
ALTER TABLE "Lead" ADD CONSTRAINT "Lead_notAppropriateById_fkey"
	FOREIGN KEY ("notAppropriateById") REFERENCES "User"(id)
	ON UPDATE CASCADE ON DELETE SET NULL;

-- Belt-and-suspenders: assignment must unassign, never wipe the lead.
ALTER TABLE "Lead" DROP CONSTRAINT IF EXISTS "Lead_assignedSalesExecId_fkey";
ALTER TABLE "Lead" ADD CONSTRAINT "Lead_assignedSalesExecId_fkey"
	FOREIGN KEY ("assignedSalesExecId") REFERENCES "User"(id)
	ON UPDATE CASCADE ON DELETE SET NULL;

ALTER TABLE "Lead" DROP CONSTRAINT IF EXISTS "Lead_assignedMainTeamLeadId_fkey";
ALTER TABLE "Lead" ADD CONSTRAINT "Lead_assignedMainTeamLeadId_fkey"
	FOREIGN KEY ("assignedMainTeamLeadId") REFERENCES "User"(id)
	ON UPDATE CASCADE ON DELETE SET NULL;
