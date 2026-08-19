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

func TestLeadFacetsInWhere(t *testing.T) {
	where, args := buildLeadListWhere(LeadListParams{
		Filter:  "all",
		Country: "Nepal",
		TeamID:  "none",
		Status:  "QUALIFIED_CALL",
	}, 0)
	sql := strings.Join(where, " AND ")
	if !strings.Contains(sql, `BTRIM(l.country)`) {
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
