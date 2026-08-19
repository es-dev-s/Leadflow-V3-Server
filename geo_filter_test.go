package main

import (
	"strings"
	"testing"
)

func TestIsBlankGeoLabel(t *testing.T) {
	cases := map[string]bool{
		"":           false,
		"Nepal":      false,
		"Unknown":    true,
		"unknown":    true,
		"NONE":       true,
		"none":       true,
		"unassigned": true,
		"blank":      true,
	}
	for in, want := range cases {
		if got := isBlankGeoLabel(in); got != want {
			t.Fatalf("isBlankGeoLabel(%q)=%v want %v", in, got, want)
		}
	}
}

func TestGeoFilterBlankCountry(t *testing.T) {
	f := parseGeoFilter("none", "")
	clause, args := f.leadClause("l.", 0)
	if len(args) != 0 {
		t.Fatalf("expected no args for blank country, got %#v", args)
	}
	if !strings.Contains(clause, "country") || !strings.Contains(clause, "NULL") {
		t.Fatalf("unexpected blank country clause: %s", clause)
	}
	if !strings.Contains(clause, "none") || !strings.Contains(clause, "unassigned") {
		t.Fatalf("blank country clause should include sentinel labels: %s", clause)
	}
}

func TestNormalizedCountrySQL(t *testing.T) {
	sql := normalizedCountrySQL("country")
	if !strings.Contains(sql, "Unknown") || !strings.Contains(sql, "BTRIM") {
		t.Fatalf("unexpected normalizedCountrySQL: %s", sql)
	}
}

func TestGeoFilterExactCity(t *testing.T) {
	f := parseGeoFilter("Nepal", "Kathmandu")
	clause, args := f.leadClause("l.", 0)
	if len(args) != 2 || args[0] != "Nepal" || args[1] != "Kathmandu" {
		t.Fatalf("args=%#v clause=%s", args, clause)
	}
	if !strings.Contains(clause, `LOWER(BTRIM(l."country"))`) {
		t.Fatalf("country match should be case-insensitive: %s", clause)
	}
}
