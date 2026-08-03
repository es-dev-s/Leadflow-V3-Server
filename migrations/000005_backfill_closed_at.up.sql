-- Stamp closedAt for already-closed leads that never got a close timestamp.
-- Best available proxy: updatedAt (last mutation when stage was set to closed).
UPDATE "Lead"
SET "closedAt" = COALESCE("closedAt", "updatedAt")
WHERE "salesStage" IN ('CLOSED_WON', 'CLOSED_LOST')
  AND "closedAt" IS NULL;
