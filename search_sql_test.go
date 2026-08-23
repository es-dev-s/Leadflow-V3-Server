package main

import (
	"strings"
	"testing"
	"time"
)

func TestGlobalSearchSQLHasNoFmtExtra(t *testing.T) {
	for _, q := range []string{"meta", "ab", "555", "john doe"} {
		where, _ := buildLeadListWhere(LeadListParams{Filter: "all", Query: q}, 0)
		sql := strings.Join(where, " AND ")
		if strings.Contains(sql, "EXTRA") || strings.Contains(sql, "%!") {
			t.Fatalf("corrupt SQL q=%q: %s", q, sql)
		}
	}
}

func TestTagSearchNewMeansUnassigned(t *testing.T) {
	where, _ := buildLeadListWhere(LeadListParams{Filter: "all", Query: "new", Field: "tag"}, 0)
	sql := strings.Join(where, " AND ")
	if !strings.Contains(sql, `l."teamId" IS NULL`) {
		t.Fatalf("tag=new should match unassigned team: %s", sql)
	}
	if !strings.Contains(sql, `l."assignedMainTeamLeadId" IS NULL`) {
		t.Fatalf("tag=new should match unassigned team lead: %s", sql)
	}
	if strings.Contains(sql, "LeadView") {
		t.Fatalf("tag=new must not use LeadView: %s", sql)
	}
}

func TestTagSearchNotAppropriateAndIrrelevant(t *testing.T) {
	where, _ := buildLeadListWhere(LeadListParams{Filter: "all", Query: "not approved", Field: "tag"}, 0)
	sql := strings.Join(where, " AND ")
	if !strings.Contains(sql, `l."notAppropriate" = TRUE`) {
		t.Fatalf("tag=not approved should match not-appropriate: %s", sql)
	}

	where, _ = buildLeadListWhere(LeadListParams{Filter: "all", Query: "Irrelevant", Field: "tag"}, 0)
	sql = strings.Join(where, " AND ")
	if !strings.Contains(sql, `l."qualificationStatus" = 'IRRELEVANT'`) {
		t.Fatalf("tag=irrelevant should match IRRELEVANT status: %s", sql)
	}
}

func TestCreatedAtBusinessDateIsListCalendarDay(t *testing.T) {
	sql := createdAtBusinessDateSQL()
	if strings.Contains(sql, `AT TIME ZONE 'UTC'`) {
		t.Fatalf("UTC wrap shifts KTM midnight leads back one graph day: %s", sql)
	}
	if !strings.Contains(sql, `AT TIME ZONE 'Asia/Kathmandu'`) {
		t.Fatalf("expected business TZ calendar day: %s", sql)
	}
	rangeSQL := leadCreatedAtInBusinessDateRangeSQL("d.bucket", "(d.bucket + INTERVAL '1 day')")
	if !strings.Contains(rangeSQL, `l."createdAt" >=`) || !strings.Contains(rangeSQL, `l."createdAt" <`) {
		t.Fatalf("series must use the same createdAt window as the list: %s", rangeSQL)
	}
}

func TestAddedDateUsesBusinessCalendar(t *testing.T) {
	where, args := buildLeadListWhere(LeadListParams{
		Filter:    "all",
		AddedFrom: "2026-08-20",
		AddedTo:   "2026-08-21",
	}, 0)
	sql := strings.Join(where, " AND ")
	if !strings.Contains(sql, `l."createdAt" >=`) {
		t.Fatalf("from date must be inclusive: %s", sql)
	}
	if !strings.Contains(sql, `l."createdAt" <`) {
		t.Fatalf("to date must be exclusive-end (inclusive calendar day): %s", sql)
	}
	if len(args) < 2 {
		t.Fatalf("expected from+to instant args, got %#v", args)
	}
	from, ok := args[0].(time.Time)
	if !ok {
		t.Fatalf("from arg should be time.Time, got %#v", args[0])
	}
	to, ok := args[1].(time.Time)
	if !ok {
		t.Fatalf("to arg should be time.Time, got %#v", args[1])
	}
	// 2026-08-20 00:00 Asia/Kathmandu == 2026-08-19 18:15 UTC
	if from.UTC().Format("2006-01-02 15:04") != "2026-08-19 18:15" {
		t.Fatalf("from should be Kathmandu midnight: %s", from.UTC())
	}
	// Exclusive end is the next calendar day 00:00 NPT (2026-08-22 00:00 NPT)
	if to.UTC().Format("2006-01-02 15:04") != "2026-08-21 18:15" {
		t.Fatalf("to exclusive-end should be next Kathmandu midnight: %s", to.UTC())
	}
}

func TestLeadFacetsInWhere(t *testing.T) {
	where, args := buildLeadListWhere(LeadListParams{
		Filter:  "all",
		Country: "Nepal",
		TeamID:  "none",
		Status:  "QUALIFIED_CALL",
	}, 0)
	sql := strings.Join(where, " AND ")
	if !strings.Contains(sql, `LOWER(BTRIM(l.country))`) {
		t.Fatalf("missing country clause: %s", sql)
	}
	if !strings.Contains(sql, `l."teamId" IS NULL`) {
		t.Fatalf("missing unassigned team clause: %s", sql)
	}
	if !strings.Contains(sql, `l."qualificationStatus"`) {
		t.Fatalf("missing status clause: %s", sql)
	}
	if len(args) < 2 {
		t.Fatalf("expected country+status args, got %#v", args)
	}
}

func TestLeadScopeWhereAppliesPresetWithoutStatus(t *testing.T) {
	sql, args := leadScopeWhere(LeadListParams{Filter: "qualified"}, true)
	if !strings.Contains(sql, `l."qualificationStatus" IN`) {
		t.Fatalf("qualified preset missing: %s %#v", sql, args)
	}
	joined := strings.Join(func() []string {
		out := make([]string, 0, len(args))
		for _, a := range args {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}(), ",")
	if !strings.Contains(joined, "QUALIFIED_CALL") {
		t.Fatalf("qualified args missing QUALIFIED_CALL: %#v", args)
	}
	if !strings.Contains(joined, "PAID") || !strings.Contains(joined, "ORGANIC") {
		t.Fatalf("qualified args missing PAID/ORGANIC: %#v", args)
	}
}

func TestLeadScopeWhereAnalystTeamLead(t *testing.T) {
	sql, args := leadScopeWhere(LeadListParams{
		AnalystTeamLeadID: "atl-1",
		AnalystTeamName:   "North Pod",
	}, true)
	if !strings.Contains(sql, `"analystTeamName"`) {
		t.Fatalf("ATL scope should match analyst team name: %s", sql)
	}
	if !strings.Contains(sql, `creator."managerId"`) {
		t.Fatalf("ATL scope should match reporting LAs: %s", sql)
	}
	foundTeam := false
	foundATL := false
	foundRole := false
	for _, a := range args {
		s, ok := a.(string)
		if !ok {
			continue
		}
		if s == "North Pod" {
			foundTeam = true
		}
		if s == "atl-1" {
			foundATL = true
		}
		if s == RoleLeadAnalyst {
			foundRole = true
		}
	}
	if !foundTeam || !foundATL || !foundRole {
		t.Fatalf("ATL scope args missing: %#v", args)
	}

	sql, args = leadScopeWhere(LeadListParams{AnalystTeamLeadID: "atl-1"}, true)
	if strings.Contains(sql, `"analystTeamName"`) {
		t.Fatalf("unlinked ATL should not match team name: %s", sql)
	}
	if !strings.Contains(sql, `creator."managerId"`) {
		t.Fatalf("unlinked ATL should still match reporting LAs: %s", sql)
	}
	_ = args
}

func TestLeadScopeWhereSkipsPresetWhenStatusSet(t *testing.T) {
	sql, args := leadScopeWhere(LeadListParams{
		Filter: "qualified",
		Status: "IRRELEVANT",
	}, true)
	joined := strings.Join(func() []string {
		out := make([]string, 0, len(args))
		for _, a := range args {
			if s, ok := a.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}(), ",")
	if strings.Contains(joined, "QUALIFIED_CALL") {
		t.Fatalf("preset must not apply when exact status is set: %s %#v", sql, args)
	}
	if !strings.Contains(sql, `l."qualificationStatus"`) {
		t.Fatalf("status facet missing: %s", sql)
	}
}

func TestClosedDateSearchUsesClosedAt(t *testing.T) {
	where, args := buildLeadListWhere(LeadListParams{
		Filter: "all",
		Query:  "2026-08-21",
		Field:  "closedDate",
	}, 0)
	sql := strings.Join(where, " AND ")
	if !strings.Contains(sql, `l."closedAt"`) {
		t.Fatalf("closedDate search must use closedAt: %s", sql)
	}
	if len(args) == 0 {
		t.Fatalf("expected date pattern arg")
	}
}

func TestClosedLabelWonLostOpen(t *testing.T) {
	now := time.Now()
	if got := closedLabel(&now, "CLOSED_LOST"); got != "Lost" {
		t.Fatalf("lost: %q", got)
	}
	if got := closedLabel(&now, "CLOSED_WON"); got != "Closed" {
		t.Fatalf("won: %q", got)
	}
	if got := closedLabel(nil, "CONTACTED"); got != "Open" {
		t.Fatalf("open: %q", got)
	}
}

func TestQuoteTimeZoneNameRejectsInjection(t *testing.T) {
	if got := quoteTimeZoneName("Asia/Kathmandu'; DROP TABLE"); got != defaultBusinessTZ {
		t.Fatalf("expected fallback, got %q", got)
	}
	if got := quoteTimeZoneName("Asia/Kathmandu"); got != "Asia/Kathmandu" {
		t.Fatalf("got %q", got)
	}
}
