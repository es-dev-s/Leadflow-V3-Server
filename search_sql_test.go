package main

import (
	"strings"
	"testing"
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

func TestAddedDateUsesBusinessCalendar(t *testing.T) {
	where, args := buildLeadListWhere(LeadListParams{
		Filter:    "all",
		AddedFrom: "2026-08-19",
		AddedTo:   "2026-08-19",
	}, 0)
	sql := strings.Join(where, " AND ")
	if !strings.Contains(sql, "AT TIME ZONE") {
		t.Fatalf("date filter must use business timezone: %s", sql)
	}
	if len(args) < 2 {
		t.Fatalf("expected tz + date args, got %#v", args)
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

func TestQuoteTimeZoneNameRejectsInjection(t *testing.T) {
	if got := quoteTimeZoneName("Asia/Kathmandu'; DROP TABLE"); got != defaultBusinessTZ {
		t.Fatalf("expected fallback, got %q", got)
	}
	if got := quoteTimeZoneName("Asia/Kathmandu"); got != "Asia/Kathmandu" {
		t.Fatalf("got %q", got)
	}
}
