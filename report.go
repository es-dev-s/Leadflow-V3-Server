package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"sort"
	"strings"
	"time"
)

// Report answers the marketing/insight questions (CDR / ACS RPL / NAATI CCL /
// PTE overview documents) from real lead data. Every number is computed from
// the Lead table with the same facet filters used across the dashboard.

// ReportBucketRow is one dimension row (country, city, source, portal, service line).
type ReportBucketRow struct {
	Name         string   `json:"name"`
	Total        int      `json:"total"`
	Qualified    int      `json:"qualified"`
	NotQualified int      `json:"notQualified"`
	Irrelevant   int      `json:"irrelevant"`
	ClosedWon    int      `json:"closedWon"`
	ClosedLost   int      `json:"closedLost"`
	Revenue      *float64 `json:"revenue,omitempty"`
}

// ReportTrendRow is one month of demand (Kathmandu calendar months).
type ReportTrendRow struct {
	Key       string `json:"key"`
	Label     string `json:"label"`
	Total     int    `json:"total"`
	Qualified int    `json:"qualified"`
	ClosedWon int    `json:"closedWon"`
}

// ReportReasonRow is a ranked free-text reason bucket.
type ReportReasonRow struct {
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// ReportTotals is the headline KPI block.
type ReportTotals struct {
	Leads                   int      `json:"leads"`
	Qualified               int      `json:"qualified"`
	NotQualified            int      `json:"notQualified"`
	Irrelevant              int      `json:"irrelevant"`
	ClosedWon               int      `json:"closedWon"`
	ClosedLost              int      `json:"closedLost"`
	Revenue                 float64  `json:"revenue"`
	AvgTimeToCloseMinutes   *int     `json:"avgTimeToCloseMinutes,omitempty"`
	AvgFirstResponseMinutes *float64 `json:"avgFirstResponseMinutes,omitempty"`
}

// ReportServiceDemand is enquiry volume for a known service/package label,
// extracted from free-text notes when no structured service field exists.
type ReportServiceDemand struct {
	Name      string  `json:"name"`
	Enquiries int     `json:"enquiries"`
	Qualified int     `json:"qualified"`
	ClosedWon int     `json:"closedWon"`
	Revenue   float64 `json:"revenue"`
	Captured  bool    `json:"captured"`
}

type ReportResponse struct {
	Totals                 ReportTotals          `json:"totals"`
	ServiceLines           []ReportBucketRow     `json:"serviceLines"`
	QualifiedCountries     []ReportBucketRow     `json:"qualifiedCountries"`
	QualifiedCities        []ReportBucketRow     `json:"qualifiedCities"`
	IrrelevantCountries    []ReportBucketRow     `json:"irrelevantCountries"`
	IrrelevantCities       []ReportBucketRow     `json:"irrelevantCities"`
	ExclusionCandidates    []ReportBucketRow     `json:"exclusionCandidates"`
	ExclusionCities        []ReportBucketRow     `json:"exclusionCities"`
	Sources                []ReportBucketRow     `json:"sources"`
	Portals                []ReportBucketRow     `json:"portals"`
	MonthlyTrend           []ReportTrendRow      `json:"monthlyTrend"`
	IrrelevantReasons      []ReportReasonRow     `json:"irrelevantReasons"`
	IrrelevantPatterns     []ReportReasonRow     `json:"irrelevantPatterns"`
	LostReasons            []ReportReasonRow     `json:"lostReasons"`
	LostOpportunityFactors []ReportReasonRow     `json:"lostOpportunityFactors"`
	ServiceDemand          []ReportServiceDemand `json:"serviceDemand"`
	LanguageDemand         []ReportServiceDemand `json:"languageDemand"`
	LanguageTrend          []ReportLanguageTrend `json:"languageTrend"`
	PromoDemand            []ReportServiceDemand `json:"promoDemand"`
}

// ReportLanguageTrend compares recent vs prior demand for a language over ~1.5 years.
type ReportLanguageTrend struct {
	Name      string   `json:"name"`
	Recent    int      `json:"recent"` // last 9 months
	Prior     int      `json:"prior"`  // months 10–18 ago
	Total18m  int      `json:"total18m"`
	GrowthPct *float64 `json:"growthPct,omitempty"`
}

// serviceLineSQL maps portal names onto the four report brands.
// ACS must match "%ACS%" only — bare "%RPL%" incorrectly matches "CDRPlanet…".
const serviceLineSQL = `
	CASE
		WHEN l."portalWebsite" ILIKE '%CCL%' OR l."portalWebsite" ILIKE '%NAATI%' THEN 'NAATI CCL'
		WHEN l."portalWebsite" ILIKE '%PTE%' THEN 'PTE'
		WHEN l."portalWebsite" ILIKE '%ACS%' THEN 'ACS RPL'
		WHEN l."portalWebsite" ILIKE '%CDR%' THEN 'CDR'
		WHEN l."portalWebsite" IS NULL OR BTRIM(l."portalWebsite") = '' THEN 'No portal recorded'
		ELSE 'Other portals'
	END
`

const reportAggColumns = `
	COUNT(*)::int,
	COUNT(*) FILTER (WHERE l."qualificationStatus" IN ('QUALIFIED','QUALIFIED_CHAT','QUALIFIED_CALL','PAID','ORGANIC'))::int,
	COUNT(*) FILTER (WHERE l."qualificationStatus" = 'NOT_QUALIFIED')::int,
	COUNT(*) FILTER (WHERE l."qualificationStatus" = 'IRRELEVANT')::int,
	COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::int,
	COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_LOST')::int,
	COALESCE(SUM(l."closedRevenue") FILTER (WHERE l."salesStage" = 'CLOSED_WON'), 0)::float8
`

func (s *LeadStore) reportBuckets(
	ctx context.Context,
	scopeSQL string,
	args []any,
	dimSQL, orderSQL, havingSQL string,
	limit int,
) ([]ReportBucketRow, error) {
	query := fmt.Sprintf(`
		SELECT %s AS name, %s
		FROM "Lead" l
		%s
		GROUP BY 1
		%s
		ORDER BY %s
		LIMIT %d`, dimSQL, reportAggColumns, scopeSQL, havingSQL, orderSQL, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ReportBucketRow, 0, limit)
	for rows.Next() {
		var r ReportBucketRow
		var revenue float64
		if err := rows.Scan(
			&r.Name, &r.Total, &r.Qualified, &r.NotQualified,
			&r.Irrelevant, &r.ClosedWon, &r.ClosedLost, &revenue,
		); err != nil {
			return nil, err
		}
		if revenue > 0 {
			r.Revenue = &revenue
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

func (s *LeadStore) reportReasons(
	ctx context.Context,
	scopeSQL string,
	args []any,
	extraWhere, reasonSQL string,
	limit int,
) ([]ReportReasonRow, error) {
	where := scopeSQL
	if where == "" {
		where = "WHERE " + extraWhere
	} else {
		where = where + " AND " + extraWhere
	}
	query := fmt.Sprintf(`
		SELECT reason, COUNT(*)::int AS cnt
		FROM (
			SELECT COALESCE(%s, 'No reason recorded') AS reason
			FROM "Lead" l
			%s
		) r
		WHERE reason <> 'No reason recorded'
		GROUP BY 1
		ORDER BY cnt DESC, reason ASC
		LIMIT %d`, reasonSQL, where, limit)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ReportReasonRow, 0, limit)
	for rows.Next() {
		var r ReportReasonRow
		if err := rows.Scan(&r.Reason, &r.Count); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// lostReasonSQL extracts the first line of SE lost notes.
const lostReasonSQL = `
	NULLIF(LEFT(BTRIM(SPLIT_PART(l."lostNotes", E'\n', 1)), 160), '')
`

// irrelevantPatternSQL classifies irrelevant leads into recurring pattern buckets
// (students, spam, wrong occupation, no response, etc.).
const irrelevantPatternSQL = `
	CASE
		WHEN COALESCE(l.notes, '') ~* 'spam|scam|fake|bot' THEN 'Spam / fake enquiries'
		WHEN COALESCE(l.notes, '') ~* 'student|studying|bachelor|university|college' THEN 'Students'
		WHEN COALESCE(l.notes, '') ~* 'wrong occupation|not engineer|unrelated|wrong service|not cdr|job search|looking for job' THEN 'Wrong occupation / unrelated services'
		WHEN COALESCE(l.notes, '') ~* 'no response|no reply|no reponse|not responding|didn.?t reply|did not reply|no answer' THEN 'No response after outreach'
		WHEN COALESCE(l.notes, '') ~* 'ad(s)?\b|advert|clicked|auto.?msg|auto.?click' THEN 'Ad curiosity / auto-click'
		WHEN COALESCE(l.notes, '') ~* 'eligib|not eligible|skill assessment' THEN 'Eligibility mismatch'
		ELSE 'Other / unspecified'
	END
`

var irrelevantPatternLabels = []string{
	"No response after outreach",
	"Ad curiosity / auto-click",
	"Spam / fake enquiries",
	"Students",
	"Wrong occupation / unrelated services",
	"Eligibility mismatch",
	"Other / unspecified",
}

// lostOpportunityFactorSQL maps closed-lost notes onto the report checklist.
const lostOpportunityFactorSQL = `
	CASE
		WHEN COALESCE(l."lostNotes", '') ~* 'price|expensive|cheap|costly|cost|discount|quote|AUD|\$' THEN 'Price'
		WHEN COALESCE(l."lostNotes", '') ~* 'competitor|another (company|agency|writer)|other (company|agency)|already (started|done|working)|went with' THEN 'Competitor comparison'
		WHEN COALESCE(l."lostNotes", '') ~* 'document|transcript|certificate|waiting for|docs' THEN 'Waiting for documents'
		WHEN COALESCE(l."lostNotes", '') ~* 'eligib|not eligible|engineers australia|skill assessment|anzsco|visa' THEN 'Engineers Australia eligibility uncertainty'
		WHEN COALESCE(l."lostNotes", '') ~* 'financ|money|budget|afford|no fund|payment|cash' THEN 'Financial constraints'
		ELSE 'Other'
	END
`

var lostOpportunityFactorLabels = []string{
	"Price",
	"Competitor comparison",
	"Waiting for documents",
	"Engineers Australia eligibility uncertainty",
	"Financial constraints",
	"Other",
}

// CCL-specific irrelevant patterns (wrong visa, unsupported language, overseas, spam…).
const cclIrrelevantPatternSQL = `
	CASE
		WHEN COALESCE(l.notes, '') ~* 'spam|scam|fake|bot|duplicate' THEN 'Spam / duplicate'
		WHEN COALESCE(l.notes, '') ~* 'visa|wrong visa|student visa|partner visa' THEN 'Wrong visa category'
		WHEN COALESCE(l.notes, '') ~* 'unsupported language|language not|no (language|course)|not available' THEN 'Unsupported language'
		WHEN COALESCE(l.notes, '') ~* 'overseas|outside|not in australia|not australia|abroad' THEN 'Overseas / outside target market'
		WHEN COALESCE(l.notes, '') ~* 'no response|no reply|no reponse|not responding|didn.?t reply|did not reply|no answer' THEN 'No response after outreach'
		WHEN COALESCE(l.notes, '') ~* 'not interested|doesn.?t need|dont need|don.?t need' THEN 'Not interested'
		WHEN COALESCE(l.notes, '') ~* 'ad(s)?\b|advert|clicked|auto.?msg|auto.?click|insta|fb msg' THEN 'Ad curiosity / auto-click'
		ELSE 'Other / unspecified'
	END
`

var cclIrrelevantPatternLabels = []string{
	"No response after outreach",
	"Not interested",
	"Ad curiosity / auto-click",
	"Spam / duplicate",
	"Wrong visa category",
	"Unsupported language",
	"Overseas / outside target market",
	"Other / unspecified",
}

// CCL closed-lost / non-conversion checklist (Q28).
const cclLostOpportunityFactorSQL = `
	CASE
		WHEN COALESCE(l."lostNotes", '') ~* 'price|expensive|budget|cost|afford' THEN 'Pricing'
		WHEN COALESCE(l."lostNotes", '') ~* 'competitor|another|somewhere else|other (institute|se|company|agency)|already enrolled|already taking|repeated client' THEN 'Comparing competitors'
		WHEN COALESCE(l."lostNotes", '') ~* 'postpon|already taken|already given|done with|has materials|doesn.?t need' THEN 'Exam postponed'
		WHEN COALESCE(l."lostNotes", '') ~* 'confidence|nervous|scared|not ready' THEN 'Lack of confidence'
		WHEN COALESCE(l."lostNotes", '') ~* 'migration|visa|pr point|skill assessment' THEN 'Waiting for migration process'
		WHEN COALESCE(l."lostNotes", '') ~* 'time|busy|later|not now|doesn.?t want to do it now|delay' THEN 'Time constraints'
		WHEN COALESCE(l."lostNotes", '') ~* 'trust|scam|fake|not sure' THEN 'Trust'
		ELSE 'Other'
	END
`

var cclLostOpportunityFactorLabels = []string{
	"Pricing",
	"Comparing competitors",
	"Exam postponed",
	"Lack of confidence",
	"Waiting for migration process",
	"Time constraints",
	"Trust",
	"Other",
}

// PTE-specific irrelevant patterns (IELTS mix-up, free material, spam…).
const pteIrrelevantPatternSQL = `
	CASE
		WHEN COALESCE(l.notes, '') ~* 'ielts' THEN 'Looking for IELTS instead of PTE'
		WHEN COALESCE(l.notes, '') ~* 'free (study )?material|free mock|free test|study material only' THEN 'Free study material only'
		WHEN COALESCE(l.notes, '') ~* 'wrong country|not for (australia|canada|uk|nz)|country requirement' THEN 'Wrong country requirements'
		WHEN COALESCE(l.notes, '') ~* 'wrong course|another course|live class|physical class|1-1|one.on.one|tutor' THEN 'Wrong course expectation'
		WHEN COALESCE(l.notes, '') ~* 'spam|scam|fake|bot|duplicate' THEN 'Spam'
		WHEN COALESCE(l.notes, '') ~* 'no response|no reply|no reponse|not responding|didn.?t reply|did not reply|no answer' THEN 'No response after outreach'
		WHEN COALESCE(l.notes, '') ~* 'not interested' THEN 'Not interested'
		WHEN COALESCE(l.notes, '') ~* 'ad(s)?\b|advert|clicked|auto.?msg|auto.?click|previous (conversation|msg|message)' THEN 'Ad curiosity / auto-click'
		ELSE 'Other / unspecified'
	END
`

var pteIrrelevantPatternLabels = []string{
	"Looking for IELTS instead of PTE",
	"Free study material only",
	"Wrong country requirements",
	"Wrong course expectation",
	"Spam",
	"No response after outreach",
	"Not interested",
	"Ad curiosity / auto-click",
	"Other / unspecified",
}

// PTE closed-lost / non-conversion checklist (Q25).
const pteLostOpportunityFactorSQL = `
	CASE
		WHEN COALESCE(l."lostNotes", '') ~* 'price|expensive|budget|cost|afford|financial' THEN 'Price'
		WHEN COALESCE(l."lostNotes", '') ~* 'competitor|another|somewhere else|other (institute|company|agency)|comparing' THEN 'Comparing competitors'
		WHEN COALESCE(l."lostNotes", '') ~* 'confidence|nervous|scared|not ready|demo' THEN 'Lack of confidence'
		WHEN COALESCE(l."lostNotes", '') ~* 'book|exam|waiting to book' THEN 'Waiting to book the exam'
		WHEN COALESCE(l."lostNotes", '') ~* 'time|busy|later|not now|delay' THEN 'Time constraints'
		WHEN COALESCE(l."lostNotes", '') ~* 'money|financ|payment|cash' THEN 'Financial reasons'
		ELSE 'Other'
	END
`

var pteLostOpportunityFactorLabels = []string{
	"Price",
	"Comparing competitors",
	"Lack of confidence",
	"Waiting to book the exam",
	"Time constraints",
	"Financial reasons",
	"Other",
}

// ACS RPL-specific irrelevant patterns.
const acsIrrelevantPatternSQL = `
	CASE
		WHEN COALESCE(l.notes, '') ~* 'spam|scam|fake|bot|duplicate' THEN 'Spam / fake enquiries'
		WHEN COALESCE(l.notes, '') ~* 'student|studying|bachelor|university|college' THEN 'Students'
		WHEN COALESCE(l.notes, '') ~* 'non.?it|not it|not (an )?it|non technical|unrelated|wrong service|looking for job|job search' THEN 'Non-IT / unrelated services'
		WHEN COALESCE(l.notes, '') ~* 'wrong visa|incorrect visa|visa pathway|canada (visa|pr|application)' THEN 'Incorrect visa pathways'
		WHEN COALESCE(l.notes, '') ~* 'no response|no reply|no reponse|not responding|didn.?t reply|did not reply|no answer' THEN 'No response after outreach'
		WHEN COALESCE(l.notes, '') ~* 'ad(s)?\b|advert|clicked|auto.?msg|auto.?click|previous (conversation|msg|message)' THEN 'Ad curiosity / auto-click'
		WHEN COALESCE(l.notes, '') ~* 'not interested|doesn.?t need|dont need|don.?t need' THEN 'Not interested'
		ELSE 'Other / unspecified'
	END
`

var acsIrrelevantPatternLabels = []string{
	"No response after outreach",
	"Ad curiosity / auto-click",
	"Spam / fake enquiries",
	"Students",
	"Non-IT / unrelated services",
	"Incorrect visa pathways",
	"Not interested",
	"Other / unspecified",
}

// ACS closed-lost / non-conversion checklist.
const acsLostOpportunityFactorSQL = `
	CASE
		WHEN COALESCE(l."lostNotes", '') ~* 'price|expensive|budget|cost|afford|free' THEN 'Pricing concerns'
		WHEN COALESCE(l."lostNotes", '') ~* 'document|employment|reference|waiting for|docs' THEN 'Waiting for employment documents'
		WHEN COALESCE(l."lostNotes", '') ~* 'eligib|not eligible|skill assessment|acs assessment' THEN 'ACS eligibility uncertainty'
		WHEN COALESCE(l."lostNotes", '') ~* 'experience|fresh|no work|lack of work' THEN 'Lack of work experience'
		WHEN COALESCE(l."lostNotes", '') ~* 'competitor|another|other agent|already (started|closed|working)|went with|garima' THEN 'Competitor comparison'
		WHEN COALESCE(l."lostNotes", '') ~* 'financ|money|payment|cash' THEN 'Financial constraints'
		ELSE 'Other'
	END
`

var acsLostOpportunityFactorLabels = []string{
	"Pricing concerns",
	"Waiting for employment documents",
	"ACS eligibility uncertainty",
	"Lack of work experience",
	"Competitor comparison",
	"Financial constraints",
	"Other",
}

func (s *LeadStore) reportClassifiedBuckets(
	ctx context.Context,
	scopeSQL string,
	args []any,
	extraWhere, classSQL string,
	labels []string,
) ([]ReportReasonRow, error) {
	where := scopeSQL
	if where == "" {
		where = "WHERE " + extraWhere
	} else {
		where = where + " AND " + extraWhere
	}
	query := fmt.Sprintf(`
		SELECT factor, COUNT(*)::int AS cnt
		FROM (
			SELECT %s AS factor
			FROM "Lead" l
			%s
		) r
		GROUP BY 1`, classSQL, where)

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	counts := make(map[string]int, len(labels))
	for rows.Next() {
		var reason string
		var count int
		if err := rows.Scan(&reason, &count); err != nil {
			return nil, err
		}
		counts[reason] = count
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	out := make([]ReportReasonRow, 0, len(labels))
	for _, label := range labels {
		out = append(out, ReportReasonRow{Reason: label, Count: counts[label]})
	}
	sort.SliceStable(out, func(i, j int) bool {
		if out[i].Count == out[j].Count {
			return out[i].Reason < out[j].Reason
		}
		return out[i].Count > out[j].Count
	})
	return out, nil
}

func (s *LeadStore) Report(ctx context.Context, params LeadListParams) (ReportResponse, error) {
	var out ReportResponse
	scopeSQL, args := leadScopeWhere(params, false)

	// Headline totals.
	totalsQuery := fmt.Sprintf(`
		SELECT %s,
			AVG(EXTRACT(EPOCH FROM (l."closedAt" - l."createdAt")) / 60.0)
				FILTER (WHERE l."closedAt" IS NOT NULL AND l."closedAt" >= l."createdAt"),
			AVG(l."firstResponseMinutes") FILTER (WHERE l."firstResponseMinutes" IS NOT NULL)
		FROM "Lead" l
		%s`, reportAggColumns, scopeSQL)
	var avgClose, avgFirst *float64
	if err := s.pool.QueryRow(ctx, totalsQuery, args...).Scan(
		&out.Totals.Leads, &out.Totals.Qualified, &out.Totals.NotQualified,
		&out.Totals.Irrelevant, &out.Totals.ClosedWon, &out.Totals.ClosedLost,
		&out.Totals.Revenue, &avgClose, &avgFirst,
	); err != nil {
		return out, fmt.Errorf("report totals: %w", err)
	}
	if avgClose != nil {
		mins := int(*avgClose + 0.5)
		out.Totals.AvgTimeToCloseMinutes = &mins
	}
	out.Totals.AvgFirstResponseMinutes = avgFirst

	countryDim := `COALESCE(NULLIF(BTRIM(l.country), ''), 'Unknown')`
	// Named cities only — blank city is excluded from city rankings.
	cityDim := `NULLIF(BTRIM(l.city), '')`
	// Use "none" so report → /leads?source=none matches appendBlankOrEqual.
	sourceDim := `COALESCE(NULLIF(BTRIM(l.source), ''), 'none')`
	portalDim := `COALESCE(NULLIF(BTRIM(l."portalWebsite"), ''), 'No portal recorded')`

	qualFilter := `COUNT(*) FILTER (WHERE l."qualificationStatus" IN ('QUALIFIED','QUALIFIED_CHAT','QUALIFIED_CALL','PAID','ORGANIC'))`
	irrelFilter := `COUNT(*) FILTER (WHERE l."qualificationStatus" = 'IRRELEVANT')`
	namedCountry := `LOWER(COALESCE(NULLIF(BTRIM(MIN(l.country)), ''), '')) NOT IN ('', 'unknown')`
	namedCity := `LOWER(COALESCE(NULLIF(BTRIM(MIN(l.city)), ''), '')) NOT IN ('', 'unknown')`
	// Exclusion: enough volume + under 0.5% qualified yield (named locations only).
	excludeHavingCountry := fmt.Sprintf(
		`HAVING COUNT(*) >= 150 AND %s::float / COUNT(*) < 0.005 AND %s`,
		qualFilter, namedCountry,
	)
	excludeHavingCity := fmt.Sprintf(
		`HAVING COUNT(*) >= 80 AND %s::float / COUNT(*) < 0.005 AND %s`,
		qualFilter, namedCity,
	)

	type bucketJob struct {
		dest   *[]ReportBucketRow
		dim    string
		order  string
		having string
		limit  int
	}
	jobs := []bucketJob{
		{&out.ServiceLines, serviceLineSQL, "2 DESC", "", 8},
		{&out.QualifiedCountries, countryDim, "3 DESC, 2 DESC",
			fmt.Sprintf(`HAVING %s > 0 AND %s`, qualFilter, namedCountry), 10},
		{&out.QualifiedCities, cityDim, "3 DESC, 2 DESC",
			fmt.Sprintf(`HAVING %s > 0 AND %s`, qualFilter, namedCity), 10},
		{&out.IrrelevantCountries, countryDim, "5 DESC",
			fmt.Sprintf(`HAVING %s > 0 AND %s`, irrelFilter, namedCountry), 10},
		{&out.IrrelevantCities, cityDim, "5 DESC",
			fmt.Sprintf(`HAVING %s > 0 AND %s`, irrelFilter, namedCity), 10},
		{&out.ExclusionCandidates, countryDim, "2 DESC", excludeHavingCountry, 10},
		{&out.ExclusionCities, cityDim, "2 DESC", excludeHavingCity, 10},
		{&out.Sources, sourceDim, "2 DESC", "", 30},
		{&out.Portals, portalDim, "2 DESC", "", 15},
	}
	for _, job := range jobs {
		rowsOut, err := s.reportBuckets(ctx, scopeSQL, args, job.dim, job.order, job.having, job.limit)
		if err != nil {
			return out, fmt.Errorf("report buckets: %w", err)
		}
		*job.dest = rowsOut
	}

	// Monthly demand trend — last 18 Kathmandu calendar months (or the
	// filtered range if narrower). Grouped in Asia/Kathmandu so months match
	// what users see everywhere else.
	trendWhere := scopeSQL
	trendCond := `l."createdAt" >= (date_trunc('month', (CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Kathmandu')::date) - INTERVAL '17 months')`
	if trendWhere == "" {
		trendWhere = "WHERE " + trendCond
	} else {
		trendWhere = trendWhere + " AND " + trendCond
	}
	trendQuery := `
		SELECT
			to_char(date_trunc('month', timezone('Asia/Kathmandu', l."createdAt"))::date, 'YYYY-MM') AS key,
			to_char(date_trunc('month', timezone('Asia/Kathmandu', l."createdAt"))::date, 'Mon YYYY') AS label,
			COUNT(*)::int,
			COUNT(*) FILTER (WHERE l."qualificationStatus" IN ('QUALIFIED','QUALIFIED_CHAT','QUALIFIED_CALL','PAID','ORGANIC'))::int,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::int
		FROM "Lead" l
		` + trendWhere + `
		GROUP BY 1, 2
		ORDER BY 1 ASC`
	trendRows, err := s.pool.Query(ctx, trendQuery, args...)
	if err != nil {
		return out, fmt.Errorf("report trend: %w", err)
	}
	defer trendRows.Close()
	out.MonthlyTrend = make([]ReportTrendRow, 0, 18)
	for trendRows.Next() {
		var r ReportTrendRow
		if err := trendRows.Scan(&r.Key, &r.Label, &r.Total, &r.Qualified, &r.ClosedWon); err != nil {
			return out, err
		}
		out.MonthlyTrend = append(out.MonthlyTrend, r)
	}
	if err := trendRows.Err(); err != nil {
		return out, err
	}

	// Irrelevant enquiry patterns — analyst-recorded reasons.
	irrelevant, err := s.reportReasons(ctx, scopeSQL, args,
		`l."qualificationStatus" = 'IRRELEVANT'`, rawQualificationReasonSQL, 12)
	if err != nil {
		return out, fmt.Errorf("report irrelevant reasons: %w", err)
	}
	out.IrrelevantReasons = irrelevant

	line := strings.ToUpper(strings.TrimSpace(params.ServiceLine))
	patternSQL, patternLabels := irrelevantPatternSQL, irrelevantPatternLabels
	factorSQL, factorLabels := lostOpportunityFactorSQL, lostOpportunityFactorLabels
	switch line {
	case "CCL":
		patternSQL, patternLabels = cclIrrelevantPatternSQL, cclIrrelevantPatternLabels
		factorSQL, factorLabels = cclLostOpportunityFactorSQL, cclLostOpportunityFactorLabels
	case "PTE":
		patternSQL, patternLabels = pteIrrelevantPatternSQL, pteIrrelevantPatternLabels
		factorSQL, factorLabels = pteLostOpportunityFactorSQL, pteLostOpportunityFactorLabels
	case "ACS":
		patternSQL, patternLabels = acsIrrelevantPatternSQL, acsIrrelevantPatternLabels
		factorSQL, factorLabels = acsLostOpportunityFactorSQL, acsLostOpportunityFactorLabels
	}

	irrelPatterns, err := s.reportClassifiedBuckets(ctx, scopeSQL, args,
		`l."qualificationStatus" = 'IRRELEVANT'`,
		patternSQL,
		patternLabels,
	)
	if err != nil {
		return out, fmt.Errorf("report irrelevant patterns: %w", err)
	}
	out.IrrelevantPatterns = irrelPatterns

	// Lost-deal reasons — SE lost notes on closed-lost leads.
	lost, err := s.reportReasons(ctx, scopeSQL, args,
		`l."salesStage" = 'CLOSED_LOST' AND l."lostNotes" IS NOT NULL AND BTRIM(l."lostNotes") <> ''`,
		lostReasonSQL, 10)
	if err != nil {
		return out, fmt.Errorf("report lost reasons: %w", err)
	}
	out.LostReasons = lost

	lostFactors, err := s.reportClassifiedBuckets(ctx, scopeSQL, args,
		`l."salesStage" = 'CLOSED_LOST' AND l."lostNotes" IS NOT NULL AND BTRIM(l."lostNotes") <> ''`,
		factorSQL,
		factorLabels,
	)
	if err != nil {
		return out, fmt.Errorf("report lost opportunity factors: %w", err)
	}
	out.LostOpportunityFactors = lostFactors

	// Service / package / language demand — keyword match (no structured fields yet).
	demand, langDemand, promoDemand, err := s.reportDemandByLine(ctx, scopeSQL, args, line)
	if err != nil {
		return out, fmt.Errorf("report service demand: %w", err)
	}
	out.ServiceDemand = demand
	out.LanguageDemand = langDemand
	out.PromoDemand = promoDemand

	if line == "CCL" {
		trend, err := s.reportLanguageTrend(ctx, scopeSQL, args, langDemand)
		if err != nil {
			return out, fmt.Errorf("report language trend: %w", err)
		}
		out.LanguageTrend = trend
	} else {
		out.LanguageTrend = []ReportLanguageTrend{}
	}

	return out, nil
}

type keywordDemandDef struct {
	name    string
	pattern string
}

func cdrServiceDefs() []keywordDemandDef {
	return []keywordDemandDef{
		{"Career Episodes Report Writing", "%career episode%"},
		{"Complete CDR Report Writing", "%complete cdr%"},
		{"Summary Statement Report Writing", "%summary statement%"},
		{"CPD (Continuing Professional Development)", "%cpd%"},
		{"Engineering CV / Resume Writing", "%resume%"},
	}
}

func cclLanguageDefs() []keywordDemandDef {
	return []keywordDemandDef{
		{"Nepali", "%nepali%"},
		{"Hindi", "%hindi%"},
		{"Punjabi", "%punjabi%"},
		{"Urdu", "%urdu%"},
		{"Bengali", "%bengali%"},
		{"Tamil", "%tamil%"},
		{"Telugu", "%telugu%"},
		{"Malayalam", "%malayalam%"},
		{"Sinhala", "%sinhala%"},
		{"Gujarati", "%gujarati%"},
		{"Marathi", "%marathi%"},
		{"Kannada", "%kannada%"},
		{"Dari", "%dari%"},
		{"Persian", "%persian%"},
		{"Arabic", "%arabic%"},
		{"Mandarin", "%mandarin%"},
		{"Cantonese", "%cantonese%"},
		{"Korean", "%korean%"},
		{"Japanese", "%japanese%"},
		{"Vietnamese", "%vietnamese%"},
		{"Spanish", "%spanish%"},
		{"French", "%french%"},
		{"Indonesian", "%indonesian%"},
		{"Tagalog", "%tagalog%"},
		{"Thai", "%thai%"},
		{"Pashto", "%pashto%"},
	}
}

func cclPackageDefs() []keywordDemandDef {
	return []keywordDemandDef{
		{"NAATI CCL Course Package", "%course package%"},
		{"Free NAATI Mock Test", "%mock test%"},
		{"NAATI CCL Test Samples", "%test sample%"},
		{"NAATI CCL Vocabulary", "%vocab%"},
		{"PTE Online Course", "%pte%"},
		{"Crash Course", "%crash course%"},
		{"Premium Package", "%premium%"},
		{"Unlimited Package", "%unlimited%"},
		{"Recent Exam Dialogues & Packages", "%dialogue%"},
	}
}

func ptePackageDefs() []keywordDemandDef {
	return []keywordDemandDef{
		{"Comprehensive Package ($399)", "%comprehensive%"},
		{"Premium Package ($249)", "%premium package%"},
		{"Essential Package ($149)", "%essential%"},
		{"Basic Package ($49)", "%basic package%"},
		{"Starter Package ($29)", "%starter%"},
		{"Coaching Intensive Package ($599)", "%coaching intensive%"},
		{"Basic Mock Test Bundle", "%basic mock%"},
		{"Standard Mock Test Bundle", "%standard mock%"},
		{"Premium Mock Test Bundle", "%premium mock%"},
	}
}

func ptePromoDefs() []keywordDemandDef {
	return []keywordDemandDef{
		{"Discount / promotional offer", "%discount%"},
		{"Save 20% PTE HUB offer", "%save 20%"},
		{"Promo / offer mention", "%promo%"},
		{"Offer mention", "%offer%"},
	}
}

func acsServiceDefs() []keywordDemandDef {
	return []keywordDemandDef{
		{"Complete RPL Report Writing", "%rpl report%"},
		{"Employment Reference Letter", "%reference letter%"},
		{"Project Report Writing", "%project report%"},
		{"Key Areas of Knowledge (KAO) Writing", "%kao%"},
		{"RPL Plagiarism Removal", "%plagiarism removal%"},
		{"RPL Editing & Proofreading", "%rpl editing%"},
		{"Project Arrangement", "%project arrangement%"},
		{"Resume Writing", "%resume%"},
		{"RPL Proofreading", "%proofread%"},
		// Prefer specific phrase — bare "%plagiarism%" is too noisy for ad decisions.
		{"RPL Plagiarism Check", "%plagiarism check%"},
	}
}

func (s *LeadStore) reportDemandByLine(
	ctx context.Context,
	scopeSQL string,
	args []any,
	serviceLine string,
) (services []ReportServiceDemand, languages []ReportServiceDemand, promo []ReportServiceDemand, err error) {
	switch serviceLine {
	case "CDR", "":
		services, err = s.reportKeywordDemand(ctx, scopeSQL, args, cdrServiceDefs())
		return services, []ReportServiceDemand{}, []ReportServiceDemand{}, err
	case "CCL":
		languages, err = s.reportKeywordDemand(ctx, scopeSQL, args, cclLanguageDefs())
		if err != nil {
			return nil, nil, nil, err
		}
		services, err = s.reportKeywordDemand(ctx, scopeSQL, args, cclPackageDefs())
		return services, languages, []ReportServiceDemand{}, err
	case "PTE":
		services, err = s.reportKeywordDemand(ctx, scopeSQL, args, ptePackageDefs())
		if err != nil {
			return nil, nil, nil, err
		}
		promo, err = s.reportKeywordDemand(ctx, scopeSQL, args, ptePromoDefs())
		return services, []ReportServiceDemand{}, promo, err
	case "ACS":
		services, err = s.reportKeywordDemand(ctx, scopeSQL, args, acsServiceDefs())
		return services, []ReportServiceDemand{}, []ReportServiceDemand{}, err
	default:
		return []ReportServiceDemand{}, []ReportServiceDemand{}, []ReportServiceDemand{}, nil
	}
}

func (s *LeadStore) reportKeywordDemand(
	ctx context.Context,
	scopeSQL string,
	args []any,
	defs []keywordDemandDef,
) ([]ReportServiceDemand, error) {
	out := make([]ReportServiceDemand, 0, len(defs))
	for _, def := range defs {
		where := scopeSQL
		clause := `(COALESCE(l.notes, '') ILIKE $%[1]d OR COALESCE(l."clientProfile", '') ILIKE $%[1]d)`
		n := len(args) + 1
		clause = fmt.Sprintf(clause, n)
		if where == "" {
			where = "WHERE " + clause
		} else {
			where = where + " AND " + clause
		}
		queryArgs := append(append([]any{}, args...), def.pattern)
		var enquiries, qualified, won int
		var revenue float64
		err := s.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT
				COUNT(*)::int,
				COUNT(*) FILTER (WHERE l."qualificationStatus" IN ('QUALIFIED','QUALIFIED_CHAT','QUALIFIED_CALL','PAID','ORGANIC'))::int,
				COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::int,
				COALESCE(SUM(l."closedRevenue") FILTER (WHERE l."salesStage" = 'CLOSED_WON'), 0)::float8
			FROM "Lead" l
			%s`, where), queryArgs...).Scan(&enquiries, &qualified, &won, &revenue)
		if err != nil {
			return nil, err
		}
		out = append(out, ReportServiceDemand{
			Name:      def.name,
			Enquiries: enquiries,
			Qualified: qualified,
			ClosedWon: won,
			Revenue:   revenue,
			Captured:  enquiries > 0,
		})
	}
	sort.SliceStable(out, func(i, j int) bool {
		return out[i].Enquiries > out[j].Enquiries
	})
	return out, nil
}

func (s *LeadStore) reportLanguageTrend(
	ctx context.Context,
	scopeSQL string,
	args []any,
	languages []ReportServiceDemand,
) ([]ReportLanguageTrend, error) {
	// Only score languages that already show enquiry signal.
	defs := make([]keywordDemandDef, 0, len(languages))
	for _, lang := range languages {
		if lang.Enquiries == 0 {
			continue
		}
		for _, def := range cclLanguageDefs() {
			if def.name == lang.Name {
				defs = append(defs, def)
				break
			}
		}
	}
	out := make([]ReportLanguageTrend, 0, len(defs))
	for _, def := range defs {
		where := scopeSQL
		clause := `(COALESCE(l.notes, '') ILIKE $%[1]d OR COALESCE(l."clientProfile", '') ILIKE $%[1]d)`
		n := len(args) + 1
		clause = fmt.Sprintf(clause, n)
		if where == "" {
			where = "WHERE " + clause
		} else {
			where = where + " AND " + clause
		}
		queryArgs := append(append([]any{}, args...), def.pattern)
		var recent, prior int
		err := s.pool.QueryRow(ctx, fmt.Sprintf(`
			SELECT
				COUNT(*) FILTER (
					WHERE timezone('Asia/Kathmandu', l."createdAt")::date
						>= ((CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Kathmandu')::date - INTERVAL '9 months')
				)::int,
				COUNT(*) FILTER (
					WHERE timezone('Asia/Kathmandu', l."createdAt")::date
						>= ((CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Kathmandu')::date - INTERVAL '18 months')
					AND timezone('Asia/Kathmandu', l."createdAt")::date
						< ((CURRENT_TIMESTAMP AT TIME ZONE 'Asia/Kathmandu')::date - INTERVAL '9 months')
				)::int
			FROM "Lead" l
			%s`, where), queryArgs...).Scan(&recent, &prior)
		if err != nil {
			return nil, err
		}
		row := ReportLanguageTrend{
			Name:     def.name,
			Recent:   recent,
			Prior:    prior,
			Total18m: recent + prior,
		}
		if prior > 0 {
			g := (float64(recent) - float64(prior)) / float64(prior) * 100
			row.GrowthPct = &g
		} else if recent > 0 {
			g := 100.0
			row.GrowthPct = &g
		}
		out = append(out, row)
	}
	sort.SliceStable(out, func(i, j int) bool {
		gi, gj := -9999.0, -9999.0
		if out[i].GrowthPct != nil {
			gi = *out[i].GrowthPct
		}
		if out[j].GrowthPct != nil {
			gj = *out[j].GrowthPct
		}
		if gi != gj {
			return gi > gj
		}
		return out[i].Recent > out[j].Recent
	})
	// Prefer rising demand; if none rising, still return top by recent volume.
	rising := make([]ReportLanguageTrend, 0, len(out))
	for _, row := range out {
		if row.GrowthPct != nil && *row.GrowthPct > 0 && row.Recent > 0 {
			rising = append(rising, row)
		}
	}
	if len(rising) > 0 {
		if len(rising) > 10 {
			rising = rising[:10]
		}
		return rising, nil
	}
	if len(out) > 10 {
		out = out[:10]
	}
	return out, nil
}

func (s *Server) handleReport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}
	if s.serveFromCache(w, r) {
		return
	}

	params := s.leadListParamsFromRequest(r)
	reqCtx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()

	report, err := s.leads.Report(reqCtx, params)
	if err != nil {
		if !strings.Contains(err.Error(), "context canceled") {
			log.Printf("report: %v", err)
		}
		writeError(w, http.StatusInternalServerError, "failed to load report")
		return
	}
	s.writeCachedJSON(w, r, report, 30*time.Second)
}
