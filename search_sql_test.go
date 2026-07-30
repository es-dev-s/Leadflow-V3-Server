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
