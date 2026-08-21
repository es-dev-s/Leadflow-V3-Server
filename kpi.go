package main

import (
	"context"
	"math"
)

// KpiSnapshot maps the 7 TODO.md KPIs onto live Lead / User counts plus targets.
type KpiSnapshot struct {
	Items               []KpiItem `json:"items"`
	NotAppropriateCount int64     `json:"notAppropriateCount"`
}

type KpiItem struct {
	ID          string `json:"id"`
	Label       string `json:"label"`
	Description string `json:"description"`
	Formula     string `json:"formula,omitempty"`
	Available   bool   `json:"available"`
	Unavailable string `json:"unavailableReason,omitempty"`

	Numerator   *int64   `json:"numerator,omitempty"`
	Denominator *int64   `json:"denominator,omitempty"`
	Rate        *float64 `json:"rate,omitempty"`
	Value       *float64 `json:"value,omitempty"`
	Unit        string   `json:"unit,omitempty"`
	Direction   string   `json:"direction,omitempty"`

	TargetValue       *float64 `json:"targetValue,omitempty"`
	BenchmarkValue    *float64 `json:"benchmarkValue,omitempty"`
	TeamWeight        *float64 `json:"teamWeight,omitempty"`
	SupervisorWeight  *float64 `json:"supervisorWeight,omitempty"`
	TeamAligned       bool     `json:"teamAligned"`
	SupervisorAligned bool     `json:"supervisorAligned"`

	MetTarget    *bool `json:"metTarget,omitempty"`
	MetBenchmark *bool `json:"metBenchmark,omitempty"`
}

func (s *LeadStore) KPI(ctx context.Context, params LeadListParams, teamHC int64) (KpiSnapshot, error) {
	targets, err := s.MapKpiTargets(ctx)
	if err != nil {
		return KpiSnapshot{}, err
	}

	var (
		qualifiedTotal      int64
		qualifiedConverted  int64
		closedLostQual      int64
		avgScore            *float64
		scoreSamples        int64
		avgFirstResponse    *float64
		firstResponseCount  int64
		notAppropriateCount int64
	)

	leadWhere, leadArgs := leadScopeWhere(params, true)
	var avgScoreVal *float64
	var avgFirstVal *float64
	err = s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (
				WHERE l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CHAT', 'QUALIFIED_CALL', 'PAID', 'ORGANIC')
			)::bigint,
			COUNT(*) FILTER (
				WHERE l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CHAT', 'QUALIFIED_CALL', 'PAID', 'ORGANIC')
				  AND l."salesStage" = 'CLOSED_WON'
			)::bigint,
			COUNT(*) FILTER (
				WHERE l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CHAT', 'QUALIFIED_CALL', 'PAID', 'ORGANIC')
				  AND l."salesStage" = 'CLOSED_LOST'
			)::bigint,
			AVG(l."leadScore") FILTER (WHERE l."leadScore" IS NOT NULL),
			COUNT(*) FILTER (WHERE l."leadScore" IS NOT NULL)::bigint,
			AVG(l."firstResponseMinutes") FILTER (WHERE l."firstResponseMinutes" IS NOT NULL),
			COUNT(*) FILTER (WHERE l."firstResponseMinutes" IS NOT NULL)::bigint,
			COUNT(*) FILTER (WHERE l."notAppropriate" = TRUE)::bigint
		FROM "Lead" l `+leadWhere, leadArgs...).Scan(
		&qualifiedTotal,
		&qualifiedConverted,
		&closedLostQual,
		&avgScoreVal,
		&scoreSamples,
		&avgFirstVal,
		&firstResponseCount,
		&notAppropriateCount,
	)
	if err != nil {
		return KpiSnapshot{}, err
	}
	avgScore = avgScoreVal
	avgFirstResponse = avgFirstVal

	// Exactly the 7 KPIs from TODO.md, in that order.
	raw := []KpiItem{
		{
			ID:          "lead_data_accuracy",
			Label:       "Lead Data Accuracy",
			Description: "Percentage of leads with complete and accurate information as per qualifying criteria",
			Formula:     "Leads qualified correctly / Total leads qualified × 100",
			Available:   false,
			Unavailable: "Needs Quality Department hygiene-audit correct/incorrect marking on leads",
			Denominator: int64Ptr(qualifiedTotal),
			Unit:        "percent",
		},
		kpiFirstResponse(avgFirstResponse, firstResponseCount),
		kpiRatio(
			"qualified_conversion",
			"Lead Conversion Rate to Sales",
			"Percentage of qualified leads that convert to sales",
			"Qualified leads converted / Total leads qualified × 100",
			qualifiedConverted,
			qualifiedTotal,
			"higher_better",
		),
		kpiRatio(
			"ineligibility_rejection",
			"Lead Rejection Rate Post Qualify",
			"Percentage of leads rejected by sales after qualify (closed lost)",
			"Closed-lost qualified leads / Total leads qualified × 100",
			closedLostQual,
			qualifiedTotal,
			"lower_better",
		),
		kpiQualityScore(avgScore, scoreSamples),
		{
			ID:          "report_accuracy",
			Label:       "Reporting and Analytics",
			Description: "Complete reporting with accuracy and insights (weekly)",
			Formula:     "Process score — 100% accurate report with minimal re-work",
			Available:   false,
			Unavailable: "Process goal — not stored as a measurable count in the CRM",
			Unit:        "percent",
		},
		{
			ID:          "team_attrition",
			Label:       "Team Attrition",
			Description: "Team members left / total team headcount (YTD)",
			Formula:     "Members left / Total team HC × 100",
			Available:   false,
			Unavailable: "No offboarding / left-date tracking on users yet",
			Denominator: int64Ptr(teamHC),
			Unit:        "percent",
		},
	}

	items := make([]KpiItem, 0, len(raw))
	for _, item := range raw {
		items = append(items, attachKpiTarget(item, targets[item.ID]))
	}
	return KpiSnapshot{
		Items:               items,
		NotAppropriateCount: notAppropriateCount,
	}, nil
}

func attachKpiTarget(item KpiItem, t KpiTargetConfig) KpiItem {
	if t.Key == "" {
		return item
	}
	if item.Label == "" {
		item.Label = t.Label
	}
	if item.Description == "" {
		item.Description = t.Description
	}
	if item.Formula == "" {
		item.Formula = t.Formula
	}
	if item.Unit == "" {
		item.Unit = t.Unit
	}
	item.Direction = t.Direction
	item.TargetValue = t.TargetValue
	item.BenchmarkValue = t.BenchmarkValue
	item.TeamWeight = t.TeamWeight
	item.SupervisorWeight = t.SupervisorWeight
	item.TeamAligned = t.TeamAligned
	item.SupervisorAligned = t.SupervisorAligned

	actual := kpiActual(item)
	if actual != nil && t.TargetValue != nil && t.Direction != "info" {
		met := kpiMeets(t.Direction, *actual, *t.TargetValue)
		item.MetTarget = &met
	}
	if actual != nil && t.BenchmarkValue != nil && t.Direction != "info" {
		met := kpiMeets(t.Direction, *actual, *t.BenchmarkValue)
		item.MetBenchmark = &met
	}
	return item
}

func kpiActual(item KpiItem) *float64 {
	if !item.Available {
		return nil
	}
	if item.Rate != nil {
		return item.Rate
	}
	if item.Value != nil {
		return item.Value
	}
	if item.Numerator != nil && (item.Unit == "count" || item.Unit == "") {
		v := float64(*item.Numerator)
		return &v
	}
	return nil
}

func kpiMeets(direction string, actual, threshold float64) bool {
	switch direction {
	case "lower_better":
		return actual <= threshold+1e-9
	case "higher_better":
		return actual+1e-9 >= threshold
	default:
		return false
	}
}

func kpiFirstResponse(avgMinutes *float64, samples int64) KpiItem {
	item := KpiItem{
		ID:          "first_response_time",
		Label:       "Agent First Response Time",
		Description: "Average time to reply to the first customer message",
		Formula:     "First agent reply − first client message",
		Available:   samples > 0 && avgMinutes != nil,
		Unit:        "minutes",
		Direction:   "lower_better",
		Denominator: int64Ptr(samples),
	}
	if !item.Available {
		item.Unavailable = "No first-response times recorded yet — add client & agent message times on the lead form"
		return item
	}
	rounded := math.Round(*avgMinutes*10) / 10
	item.Value = &rounded
	return item
}

func kpiRatio(id, label, description, formula string, num, den int64, direction string) KpiItem {
	if direction == "" {
		direction = "higher_better"
	}
	item := KpiItem{
		ID:          id,
		Label:       label,
		Description: description,
		Formula:     formula,
		Available:   true,
		Numerator:   int64Ptr(num),
		Denominator: int64Ptr(den),
		Unit:        "percent",
		Direction:   direction,
	}
	if den > 0 {
		rate := math.Round((float64(num)/float64(den))*1000) / 10
		item.Rate = &rate
	}
	return item
}

func kpiQualityScore(avg *float64, samples int64) KpiItem {
	item := KpiItem{
		ID:          "quality_score",
		Label:       "Quality Score",
		Description: "Aggregate quality score as per audit / lead score",
		Formula:     "AVG(leadScore)",
		Available:   samples > 0 && avg != nil,
		Unit:        "score",
		Direction:   "higher_better",
		Denominator: int64Ptr(samples),
	}
	if !item.Available {
		item.Unavailable = "No lead scores recorded yet"
		return item
	}
	rounded := math.Round(*avg*10) / 10
	item.Value = &rounded
	return item
}

func int64Ptr(v int64) *int64 { return &v }
