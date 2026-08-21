-- Catalog of qualification statuses. Lead.qualificationStatus stays TEXT so
-- existing rows are untouched; the app validates writes against this set.
CREATE TABLE IF NOT EXISTS "QualificationStatusCatalog" (
	"code" TEXT PRIMARY KEY,
	"label" TEXT NOT NULL,
	"assignable" BOOLEAN NOT NULL DEFAULT FALSE,
	"sortOrder" INT NOT NULL
);

INSERT INTO "QualificationStatusCatalog" ("code", "label", "assignable", "sortOrder") VALUES
	('QUALIFIED', 'Qualified', TRUE, 10),
	('QUALIFIED_CHAT', 'Qualified - Chat', TRUE, 20),
	('QUALIFIED_CALL', 'Qualified - Call', TRUE, 30),
	('PAID', 'Paid', TRUE, 40),
	('ORGANIC', 'Organic', TRUE, 50),
	('NOT_QUALIFIED', 'Not Qualified', FALSE, 60),
	('IRRELEVANT', 'Irrelevant', FALSE, 70)
ON CONFLICT ("code") DO UPDATE SET
	"label" = EXCLUDED."label",
	"assignable" = EXCLUDED."assignable",
	"sortOrder" = EXCLUDED."sortOrder";
