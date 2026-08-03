-- Superadmin-editable KPI target configuration.

CREATE TABLE IF NOT EXISTS "KpiTarget" (
	key TEXT PRIMARY KEY,
	label TEXT NOT NULL,
	description TEXT NOT NULL DEFAULT '',
	formula TEXT NOT NULL DEFAULT '',
	unit TEXT NOT NULL DEFAULT 'percent',
	direction TEXT NOT NULL DEFAULT 'higher_better',
	"targetValue" DOUBLE PRECISION,
	"benchmarkValue" DOUBLE PRECISION,
	"teamWeight" DOUBLE PRECISION,
	"supervisorWeight" DOUBLE PRECISION,
	"teamAligned" BOOLEAN NOT NULL DEFAULT TRUE,
	"supervisorAligned" BOOLEAN NOT NULL DEFAULT TRUE,
	"sortOrder" INTEGER NOT NULL DEFAULT 0,
	"updatedAt" TIMESTAMPTZ NOT NULL DEFAULT NOW()
);
