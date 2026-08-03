-- Soft-disable accounts without deleting CRM data.
ALTER TABLE "User"
  ADD COLUMN IF NOT EXISTS "isActive" boolean NOT NULL DEFAULT TRUE;
