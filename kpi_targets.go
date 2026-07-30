package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Canonical TODO.md KPI keys (exactly 7).
var canonicalKpiKeys = []string{
	"lead_data_accuracy",
	"first_response_time",
	"qualified_conversion",
	"ineligibility_rejection",
	"quality_score",
	"report_accuracy",
	"team_attrition",
}

// KpiTargetConfig is a Superadmin-editable target row for one KPI metric.
type KpiTargetConfig struct {
	Key               string    `json:"key"`
	Label             string    `json:"label"`
	Description       string    `json:"description"`
	Formula           string    `json:"formula,omitempty"`
	Unit              string    `json:"unit"`      // percent | minutes | hours | count | score
	Direction         string    `json:"direction"` // higher_better | lower_better | info
	TargetValue       *float64  `json:"targetValue"`
	BenchmarkValue    *float64  `json:"benchmarkValue"`
	TeamWeight        *float64  `json:"teamWeight"`
	SupervisorWeight  *float64  `json:"supervisorWeight"`
	TeamAligned       bool      `json:"teamAligned"`
	SupervisorAligned bool      `json:"supervisorAligned"`
	SortOrder         int       `json:"sortOrder"`
	UpdatedAt         time.Time `json:"updatedAt"`
}

type kpiTargetSeed struct {
	key, label, description, formula, unit, direction string
	target, benchmark, teamW, supW                    *float64
	teamAligned, supervisorAligned                    bool
	sort                                              int
}

func f64(v float64) *float64 { return &v }

func defaultKpiTargetSeeds() []kpiTargetSeed {
	// Only the 7 KPIs listed in TODO.md.
	return []kpiTargetSeed{
		{
			key: "lead_data_accuracy", label: "Lead Data Accuracy",
			description: "Percentage of leads with complete and accurate information as per qualifying criteria (Quality hygiene audit)",
			formula:     "Leads qualified correctly / Total leads qualified × 100",
			unit:        "percent", direction: "higher_better",
			target: f64(95), benchmark: f64(85.5),
			teamW: f64(20), supW: f64(20),
			teamAligned: true, supervisorAligned: true, sort: 10,
		},
		{
			key: "first_response_time", label: "Agent First Response Time",
			description: "Average time to reply to the first customer message (business hours)",
			formula:     "First agent reply − first client message",
			unit:        "minutes", direction: "lower_better",
			target: f64(5), benchmark: f64(5.5),
			teamW: f64(20), supW: f64(20),
			teamAligned: true, supervisorAligned: true, sort: 20,
		},
		{
			key: "qualified_conversion", label: "Lead Conversion Rate to Sales",
			description: "Percentage of qualified leads that convert to sales",
			formula:     "Qualified leads converted / Total leads qualified × 100",
			unit:        "percent", direction: "higher_better",
			target: f64(25), benchmark: f64(22.5),
			teamW: f64(20), supW: f64(15),
			teamAligned: true, supervisorAligned: true, sort: 30,
		},
		{
			key: "ineligibility_rejection", label: "Lead Rejection Rate Post Qualify",
			description: "Percentage of leads rejected by sales due to ineligibility",
			formula:     "Leads closed lost after qualify / Total leads qualified × 100",
			unit:        "percent", direction: "lower_better",
			target: f64(5), benchmark: f64(5.5),
			teamW: f64(20), supW: f64(15),
			teamAligned: true, supervisorAligned: true, sort: 40,
		},
		{
			key: "quality_score", label: "Quality Score",
			description: "Aggregate quality score as per audit / lead score",
			formula:     "AVG(leadScore)",
			unit:        "score", direction: "higher_better",
			target: f64(95), benchmark: f64(85.5),
			teamW: f64(20), supW: f64(15),
			teamAligned: true, supervisorAligned: true, sort: 50,
		},
		{
			key: "report_accuracy", label: "Reporting and Analytics",
			description: "Complete reporting with accuracy and insights (weekly)",
			formula:     "Process score — 100% accurate report with minimal re-work",
			unit:        "percent", direction: "higher_better",
			target: f64(100), benchmark: f64(90),
			supW:              f64(10),
			teamAligned:       false,
			supervisorAligned: true, sort: 60,
		},
		{
			key: "team_attrition", label: "Team Attrition",
			description: "Team members left / total team headcount (YTD)",
			formula:     "Members left / Total team HC × 100",
			unit:        "percent", direction: "lower_better",
			target: f64(10), benchmark: f64(11),
			supW:              f64(5),
			teamAligned:       false,
			supervisorAligned: true, sort: 70,
		},
	}
}

func (s *LeadStore) EnsureKpiTargetSchema(ctx context.Context) error {
	_, err := s.pool.Exec(ctx, `
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
		)`)
	if err != nil {
		return fmt.Errorf("create KpiTarget: %w", err)
	}

	now := time.Now().UTC()
	for _, seed := range defaultKpiTargetSeeds() {
		_, err := s.pool.Exec(ctx, `
			INSERT INTO "KpiTarget" (
				key, label, description, formula, unit, direction,
				"targetValue", "benchmarkValue", "teamWeight", "supervisorWeight",
				"teamAligned", "supervisorAligned", "sortOrder", "updatedAt"
			) VALUES (
				$1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11,$12,$13,$14
			)
			ON CONFLICT (key) DO UPDATE SET
				label = EXCLUDED.label,
				description = EXCLUDED.description,
				formula = EXCLUDED.formula,
				unit = EXCLUDED.unit,
				direction = EXCLUDED.direction,
				"teamAligned" = EXCLUDED."teamAligned",
				"supervisorAligned" = EXCLUDED."supervisorAligned",
				"sortOrder" = EXCLUDED."sortOrder"`,
			seed.key, seed.label, seed.description, seed.formula, seed.unit, seed.direction,
			seed.target, seed.benchmark, seed.teamW, seed.supW,
			seed.teamAligned, seed.supervisorAligned, seed.sort, now,
		)
		if err != nil {
			return fmt.Errorf("seed KpiTarget %s: %w", seed.key, err)
		}
	}

	// Drop legacy KPIs that are not in TODO.md (keep only the 7).
	placeholders := make([]string, len(canonicalKpiKeys))
	args := make([]any, len(canonicalKpiKeys))
	for i, key := range canonicalKpiKeys {
		placeholders[i] = fmt.Sprintf("$%d", i+1)
		args[i] = key
	}
	_, _ = s.pool.Exec(ctx,
		`DELETE FROM "KpiTarget" WHERE key NOT IN (`+strings.Join(placeholders, ",")+`)`,
		args...,
	)
	return nil
}

func (s *LeadStore) ListKpiTargets(ctx context.Context) ([]KpiTargetConfig, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT
			key, label, description, formula, unit, direction,
			"targetValue", "benchmarkValue", "teamWeight", "supervisorWeight",
			"teamAligned", "supervisorAligned", "sortOrder", "updatedAt"
		FROM "KpiTarget"
		WHERE key = ANY($1)
		ORDER BY "sortOrder" ASC, key ASC`, canonicalKpiKeys)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]KpiTargetConfig, 0, 7)
	for rows.Next() {
		var t KpiTargetConfig
		if err := rows.Scan(
			&t.Key, &t.Label, &t.Description, &t.Formula, &t.Unit, &t.Direction,
			&t.TargetValue, &t.BenchmarkValue, &t.TeamWeight, &t.SupervisorWeight,
			&t.TeamAligned, &t.SupervisorAligned, &t.SortOrder, &t.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *LeadStore) MapKpiTargets(ctx context.Context) (map[string]KpiTargetConfig, error) {
	list, err := s.ListKpiTargets(ctx)
	if err != nil {
		return nil, err
	}
	m := make(map[string]KpiTargetConfig, len(list))
	for _, t := range list {
		m[t.Key] = t
	}
	return m, nil
}

// KpiTargetUpdate is a Superadmin patch for numeric target fields.
type KpiTargetUpdate struct {
	Key              string   `json:"key"`
	TargetValue      *float64 `json:"targetValue"`
	BenchmarkValue   *float64 `json:"benchmarkValue"`
	TeamWeight       *float64 `json:"teamWeight"`
	SupervisorWeight *float64 `json:"supervisorWeight"`
}

func (s *LeadStore) UpdateKpiTargets(ctx context.Context, updates []KpiTargetUpdate) error {
	if len(updates) == 0 {
		return fmt.Errorf("no targets to update")
	}
	allowed := make(map[string]struct{}, len(canonicalKpiKeys))
	for _, k := range canonicalKpiKeys {
		allowed[k] = struct{}{}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	now := time.Now().UTC()
	for _, u := range updates {
		key := strings.TrimSpace(u.Key)
		if key == "" {
			return fmt.Errorf("target key is required")
		}
		if _, ok := allowed[key]; !ok {
			return fmt.Errorf("unknown KPI target key: %s", key)
		}
		if u.TeamWeight != nil && (*u.TeamWeight < 0 || *u.TeamWeight > 100) {
			return fmt.Errorf("%s: teamWeight must be between 0 and 100", key)
		}
		if u.SupervisorWeight != nil && (*u.SupervisorWeight < 0 || *u.SupervisorWeight > 100) {
			return fmt.Errorf("%s: supervisorWeight must be between 0 and 100", key)
		}
		tag, err := tx.Exec(ctx, `
			UPDATE "KpiTarget"
			SET
				"targetValue" = $2,
				"benchmarkValue" = $3,
				"teamWeight" = $4,
				"supervisorWeight" = $5,
				"updatedAt" = $6
			WHERE key = $1`,
			key, u.TargetValue, u.BenchmarkValue, u.TeamWeight, u.SupervisorWeight, now,
		)
		if err != nil {
			return err
		}
		if tag.RowsAffected() == 0 {
			return fmt.Errorf("unknown KPI target key: %s", key)
		}
	}
	return tx.Commit(ctx)
}
