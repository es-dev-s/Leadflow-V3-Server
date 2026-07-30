// Command seedleads bulk-inserts synthetic Lead rows for resilience testing.
//
// Usage (from backend/):
//
//	go run ./cmd/seedleads -target 1000000
//	go run ./cmd/seedleads -count 50000
//	go run ./cmd/seedleads -target 1000000 -fast=false
//
// Leads are randomly attributed to existing LEAD_ANALYST creators and, when
// qualified, randomly assigned to MAIN_TEAM_LEAD / SALES_EXECUTIVE (+ team).
// Rows are tagged with notes "[seed-bulk]" so they can be identified later.
package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"math/rand"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

type seAssignee struct {
	ID     string
	TeamID string
	MTLID  *string
}

type mtlAssignee struct {
	ID     string
	TeamID string
}

func loadEnvFile(path string) {
	b, err := os.ReadFile(path)
	if err != nil {
		return
	}
	for _, line := range strings.Split(string(b), "\n") {
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		k, v, ok := strings.Cut(line, "=")
		if !ok {
			continue
		}
		k = strings.TrimSpace(k)
		v = strings.Trim(strings.TrimSpace(v), `"'`)
		if os.Getenv(k) == "" {
			_ = os.Setenv(k, v)
		}
	}
}

func main() {
	countFlag := flag.Int("count", 0, "exact number of leads to insert (overrides -target)")
	targetFlag := flag.Int("target", 1_000_000, "ensure at least this many leads exist in total")
	batchFlag := flag.Int("batch", 5_000, "rows per COPY batch")
	fastFlag := flag.Bool("fast", true, "drop/rebuild secondary Lead indexes around the seed (much faster for large loads)")
	seedFlag := flag.Int64("seed", time.Now().UnixNano(), "RNG seed")
	flag.Parse()

	// Resolve .env next to module root (cwd usually backend/).
	loadEnvFile(".env")
	if os.Getenv("DATABASE_URL") == "" {
		loadEnvFile(filepath.Join("backend", ".env"))
	}
	dbURL := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dbURL == "" {
		log.Fatal("DATABASE_URL is required")
	}

	rng := rand.New(rand.NewSource(*seedFlag))
	ctx := context.Background()

	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		log.Fatalf("parse DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 8
	cfg.MinConns = 2
	cfg.ConnConfig.RuntimeParams["statement_timeout"] = "0"
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		log.Fatalf("connect: %v", err)
	}
	defer pool.Close()

	if err := pool.Ping(ctx); err != nil {
		log.Fatalf("ping: %v", err)
	}

	var existing int64
	if err := pool.QueryRow(ctx, `SELECT COUNT(*) FROM "Lead"`).Scan(&existing); err != nil {
		log.Fatalf("count leads: %v", err)
	}

	toInsert := *countFlag
	if toInsert <= 0 {
		need := int64(*targetFlag) - existing
		if need <= 0 {
			log.Printf("already have %d leads (>= target %d); nothing to do", existing, *targetFlag)
			return
		}
		toInsert = int(need)
	}
	log.Printf("existing leads=%d; inserting %d (seed=%d, batch=%d, fast=%v)",
		existing, toInsert, *seedFlag, *batchFlag, *fastFlag)

	analysts, err := loadIDs(ctx, pool, `SELECT id FROM "User" WHERE role = 'LEAD_ANALYST' ORDER BY "createdAt"`)
	if err != nil {
		log.Fatalf("load analysts: %v", err)
	}
	if len(analysts) == 0 {
		analysts, err = loadIDs(ctx, pool, `SELECT id FROM "User" WHERE role = 'SUPERADMIN' ORDER BY "createdAt" LIMIT 1`)
		if err != nil || len(analysts) == 0 {
			log.Fatal("no LEAD_ANALYST or SUPERADMIN users available to own seeded leads")
		}
		log.Printf("warning: no analysts found; using SUPERADMIN as createdBy")
	}

	ses, err := loadSEs(ctx, pool)
	if err != nil {
		log.Fatalf("load sales executives: %v", err)
	}
	mtls, err := loadMTLs(ctx, pool)
	if err != nil {
		log.Fatalf("load main team leads: %v", err)
	}
	log.Printf("pool sizes: analysts=%d ses=%d mtls=%d", len(analysts), len(ses), len(mtls))
	if len(ses) == 0 && len(mtls) == 0 {
		log.Fatal("need at least one MAIN_TEAM_LEAD or SALES_EXECUTIVE with a teamId for assignment")
	}

	var indexDefs []indexDef
	if *fastFlag && toInsert >= 10_000 {
		indexDefs, err = snapshotSecondaryIndexes(ctx, pool)
		if err != nil {
			log.Fatalf("snapshot indexes: %v", err)
		}
		log.Printf("dropping %d secondary Lead indexes for fast load…", len(indexDefs))
		if err := dropIndexes(ctx, pool, indexDefs); err != nil {
			log.Fatalf("drop indexes: %v", err)
		}
		defer func() {
			log.Printf("rebuilding %d secondary Lead indexes…", len(indexDefs))
			if err := recreateIndexes(ctx, pool, indexDefs); err != nil {
				log.Printf("ERROR rebuilding indexes: %v", err)
				log.Printf("re-run CREATE INDEX statements manually if needed")
			} else {
				log.Printf("indexes rebuilt")
			}
		}()
	}

	columns := []string{
		"id", "leadName", "phone", "leadEmail", "country", "city",
		"source", "sourceMetaProfileName", "notes",
		"qualificationStatus", "leadScore", "salesStage",
		"createdById", "assignedMainTeamLeadId", "teamId", "assignedSalesExecId",
		"execAssignedAt", "internalReassignCount",
		"createdAt", "updatedAt", "dealCurrency",
		"portalWebsite", "language", "clientProfile",
		"estimatedDealValue", "closedRevenue", "initialPayment", "closedAt",
	}

	started := time.Now()
	var inserted int
	batch := *batchFlag
	if batch < 500 {
		batch = 500
	}
	if batch > 20_000 {
		batch = 20_000
	}

	phoneBase := time.Now().Unix() % 90_000_000
	for inserted < toInsert {
		n := batch
		if remaining := toInsert - inserted; remaining < n {
			n = remaining
		}
		rows := make([][]any, n)
		now := time.Now().UTC()
		for i := 0; i < n; i++ {
			global := inserted + i
			rows[i] = buildLeadRow(rng, analysts, ses, mtls, phoneBase, global, now)
		}

		_, err := pool.CopyFrom(ctx, pgx.Identifier{"Lead"}, columns, pgx.CopyFromRows(rows))
		if err != nil {
			log.Fatalf("copy batch at offset %d: %v", inserted, err)
		}
		inserted += n
		if inserted%50_000 == 0 || inserted == toInsert {
			elapsed := time.Since(started).Seconds()
			rate := float64(inserted) / elapsed
			log.Printf("progress %d/%d (%.0f rows/s, elapsed %.1fs)", inserted, toInsert, rate, elapsed)
		}
	}

	var total int64
	_ = pool.QueryRow(ctx, `SELECT COUNT(*) FROM "Lead"`).Scan(&total)
	log.Printf("done: inserted=%d total_leads=%d in %s", inserted, total, time.Since(started).Round(time.Millisecond))
}

func loadIDs(ctx context.Context, pool *pgxpool.Pool, sql string) ([]string, error) {
	rows, err := pool.Query(ctx, sql)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]string, 0, 64)
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

func loadSEs(ctx context.Context, pool *pgxpool.Pool) ([]seAssignee, error) {
	rows, err := pool.Query(ctx, `
		SELECT
			u.id,
			u."teamId",
			(
				SELECT m.id
				FROM "User" m
				WHERE m.role = 'MAIN_TEAM_LEAD'
				  AND m."teamId" = u."teamId"
				ORDER BY m."createdAt" ASC
				LIMIT 1
			) AS mtl_id
		FROM "User" u
		WHERE u.role = 'SALES_EXECUTIVE'
		  AND u."teamId" IS NOT NULL
		ORDER BY u."createdAt"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]seAssignee, 0, 128)
	for rows.Next() {
		var row seAssignee
		if err := rows.Scan(&row.ID, &row.TeamID, &row.MTLID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

func loadMTLs(ctx context.Context, pool *pgxpool.Pool) ([]mtlAssignee, error) {
	rows, err := pool.Query(ctx, `
		SELECT id, "teamId"
		FROM "User"
		WHERE role = 'MAIN_TEAM_LEAD'
		  AND "teamId" IS NOT NULL
		ORDER BY "createdAt"`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]mtlAssignee, 0, 32)
	for rows.Next() {
		var row mtlAssignee
		if err := rows.Scan(&row.ID, &row.TeamID); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

type indexDef struct {
	Name string
	Def  string
}

func snapshotSecondaryIndexes(ctx context.Context, pool *pgxpool.Pool) ([]indexDef, error) {
	rows, err := pool.Query(ctx, `
		SELECT indexname, indexdef
		FROM pg_indexes
		WHERE schemaname = 'public'
		  AND tablename = 'Lead'
		  AND indexname <> 'Lead_pkey'
		ORDER BY indexname`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]indexDef, 0, 32)
	for rows.Next() {
		var d indexDef
		if err := rows.Scan(&d.Name, &d.Def); err != nil {
			return nil, err
		}
		out = append(out, d)
	}
	return out, rows.Err()
}

func dropIndexes(ctx context.Context, pool *pgxpool.Pool, defs []indexDef) error {
	for _, d := range defs {
		if _, err := pool.Exec(ctx, fmt.Sprintf(`DROP INDEX IF EXISTS %s`, pgx.Identifier{d.Name}.Sanitize())); err != nil {
			return fmt.Errorf("%s: %w", d.Name, err)
		}
	}
	return nil
}

func recreateIndexes(ctx context.Context, pool *pgxpool.Pool, defs []indexDef) error {
	for _, d := range defs {
		log.Printf("  create %s", d.Name)
		if _, err := pool.Exec(ctx, d.Def); err != nil {
			return fmt.Errorf("%s: %w", d.Name, err)
		}
	}
	return nil
}

var (
	firstNames = []string{
		"Aarav", "Priya", "Noah", "Mia", "Liam", "Sofia", "Arjun", "Emma",
		"Omar", "Zara", "Ethan", "Ava", "Kabir", "Isla", "Leo", "Amara",
		"Ryan", "Nina", "Diego", "Hana", "Samir", "Lara", "Kai", "Noor",
	}
	lastNames = []string{
		"Sharma", "Patel", "Khan", "Chen", "Garcia", "Nguyen", "Silva", "Brown",
		"Ali", "Kim", "Rossi", "Martin", "Singh", "Lopez", "Wilson", "Ahmed",
		"Park", "Costa", "Murphy", "Ivanov", "Tanaka", "Dubois", "Anders", "Lee",
	}
	sources = []string{
		"Meta WhatsApp",
		"Meta Messenger",
		"Website WhatsApp",
		"Meta Lead Form",
		"Website Download Form",
		"G.WhatsApp(CAM/CWA/CRW)",
		"Google LeadForm",
	}
	quals = []string{
		"QUALIFIED", "QUALIFIED_CHAT", "QUALIFIED_CALL",
		"NOT_QUALIFIED", "IRRELEVANT",
	}
	// Weighted via pickQual.
	countries = []struct{ Country, City, Dial string }{
		{"Australia", "Sydney", "61"},
		{"Australia", "Melbourne", "61"},
		{"Nepal", "Kathmandu", "977"},
		{"India", "Mumbai", "91"},
		{"India", "Delhi", "91"},
		{"United Arab Emirates", "Dubai", "971"},
		{"United Kingdom", "London", "44"},
		{"United States", "New York", "1"},
		{"Bangladesh", "Dhaka", "880"},
		{"Singapore", "Singapore", "65"},
	}
	portals = []string{
		"facebook.com", "instagram.com", "google.com", "company-site.com", "linkedin.com",
	}
	languages = []string{"English", "Hindi", "Nepali", "Arabic", "Spanish"}
	profiles  = []string{"Investor", "First home buyer", "NRI", "Student", "Business owner"}
	stagesSE  = []string{
		"WITH_EXECUTIVE", "IN_NEGOTIATION", "NOT_CONNECTED", "NO_RESPONSE",
		"CLOSED_WON", "CLOSED_LOST",
	}
)

func pickQual(rng *rand.Rand) string {
	// ~60% assignable / qualified family, rest NQ/IR.
	switch rng.Intn(10) {
	case 0, 1, 2, 3:
		return "QUALIFIED"
	case 4, 5:
		return "QUALIFIED_CHAT"
	case 6:
		return "QUALIFIED_CALL"
	case 7, 8:
		return "NOT_QUALIFIED"
	default:
		return "IRRELEVANT"
	}
}

func isAssignable(q string) bool {
	return q == "QUALIFIED" || q == "QUALIFIED_CHAT" || q == "QUALIFIED_CALL"
}

func buildLeadRow(
	rng *rand.Rand,
	analysts []string,
	ses []seAssignee,
	mtls []mtlAssignee,
	phoneBase int64,
	idx int,
	now time.Time,
) []any {
	qual := pickQual(rng)
	score := 20 + rng.Intn(81)
	geo := countries[rng.Intn(len(countries))]
	name := fmt.Sprintf("%s %s %d",
		firstNames[rng.Intn(len(firstNames))],
		lastNames[rng.Intn(len(lastNames))],
		idx%100000,
	)
	phoneDigits := phoneBase + int64(idx)
	phone := fmt.Sprintf("+%s %d", geo.Dial, 10000000+(phoneDigits%80000000))
	email := fmt.Sprintf("seed.lead.%d@example.invalid", idx)
	createdBy := analysts[rng.Intn(len(analysts))]
	// Spread createdAt over ~18 months.
	createdAt := now.Add(-time.Duration(rng.Intn(540*24)) * time.Hour).UTC()
	updatedAt := createdAt.Add(time.Duration(rng.Intn(72)) * time.Hour)
	if updatedAt.After(now) {
		updatedAt = now
	}

	var (
		teamID, mtlID, seID *string
		execAssignedAt      *time.Time
		stage               = "PRE_SALES"
		estDeal, closedRev, initialPay *float64
		closedAt            *time.Time
	)

	if isAssignable(qual) && (len(ses) > 0 || len(mtls) > 0) {
		// 75% SE assignment, 25% MTL-only queue when both available.
		useSE := len(ses) > 0 && (len(mtls) == 0 || rng.Intn(4) != 0)
		if useSE {
			se := ses[rng.Intn(len(ses))]
			seID = &se.ID
			teamID = &se.TeamID
			mtlID = se.MTLID
			t := createdAt.Add(time.Duration(rng.Intn(48)) * time.Hour)
			execAssignedAt = &t
			stage = stagesSE[rng.Intn(len(stagesSE))]
			if stage == "CLOSED_WON" {
				v := float64(5000 + rng.Intn(95000))
				closedRev = &v
				estDeal = &v
				ip := v * (0.1 + rng.Float64()*0.3)
				initialPay = &ip
				c := t.Add(time.Duration(rng.Intn(30*24)) * time.Hour)
				closedAt = &c
			} else if stage == "CLOSED_LOST" {
				c := t.Add(time.Duration(rng.Intn(30*24)) * time.Hour)
				closedAt = &c
			} else if stage == "IN_NEGOTIATION" {
				v := float64(8000 + rng.Intn(60000))
				estDeal = &v
			}
		} else {
			mtl := mtls[rng.Intn(len(mtls))]
			mtlID = &mtl.ID
			teamID = &mtl.TeamID
			stage = "WITH_TEAM_LEAD"
		}
	}

	meta := fmt.Sprintf("Seed Profile %d", idx%500)
	notes := fmt.Sprintf("[seed-bulk] synthetic resilience lead #%d", idx)
	portal := portals[rng.Intn(len(portals))]
	lang := languages[rng.Intn(len(languages))]
	profile := profiles[rng.Intn(len(profiles))]
	source := sources[rng.Intn(len(sources))]
	_ = quals // kept for clarity / future weighting tweaks

	return []any{
		uuid.NewString(),
		name,
		phone,
		email,
		geo.Country,
		geo.City,
		source,
		meta,
		notes,
		qual,
		score,
		stage,
		createdBy,
		mtlID,
		teamID,
		seID,
		execAssignedAt,
		0,
		createdAt,
		updatedAt,
		"AUD",
		portal,
		lang,
		profile,
		estDeal,
		closedRev,
		initialPay,
		closedAt,
	}
}
