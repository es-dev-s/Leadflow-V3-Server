package main

import (
	"strings"
	"testing"
)

func TestPaidAndOrganicAreAssignableQualified(t *testing.T) {
	for _, code := range []string{QualPaid, QualOrganic, QualQualified, QualQualifiedChat, QualQualifiedCall} {
		if !isAllowedQualification(code) {
			t.Fatalf("%s must be an allowed qualification", code)
		}
		if !isAssignableQualification(code) {
			t.Fatalf("%s must count as qualified (assignable)", code)
		}
	}
	for _, alias := range []string{"Paid", "paid", "Organic", "organic"} {
		if !isAssignableQualification(alias) {
			t.Fatalf("%q must be treated as assignable qualified", alias)
		}
		code, ok := canonicalizeQualification(alias)
		if !ok {
			t.Fatalf("%q must canonicalize", alias)
		}
		if code != QualPaid && code != QualOrganic {
			t.Fatalf("%q canonicalized to %s", alias, code)
		}
	}
	if isAssignableQualification(QualNotQualified) || isAssignableQualification(QualIrrelevant) {
		t.Fatal("not-qualified / irrelevant must not be assignable")
	}
}

func TestAssignSQLAcceptsPaidOrganicCaseInsensitive(t *testing.T) {
	sql := sqlInAssignableQualificationFold(`"qualificationStatus"`)
	if !strings.Contains(sql, "UPPER(BTRIM(") {
		t.Fatalf("assign SQL must fold case: %s", sql)
	}
	for _, code := range []string{QualPaid, QualOrganic, QualQualifiedChat} {
		if !strings.Contains(sql, "'"+code+"'") {
			t.Fatalf("assign SQL missing %s: %s", code, sql)
		}
	}
}

func TestQualificationDisplayPaidOrganic(t *testing.T) {
	if got := qualificationDisplay(QualPaid); got != "Paid" {
		t.Fatalf("Paid label: %q", got)
	}
	if got := qualificationDisplay(QualOrganic); got != "Organic" {
		t.Fatalf("Organic label: %q", got)
	}
	if got := qualificationDisplay(QualQualifiedChat); got != "Qualified - Chat" {
		t.Fatalf("chat label: %q", got)
	}
}

func TestAssignableSQLListIncludesNewStatuses(t *testing.T) {
	sql := sqlInAssignableQualification(`l."qualificationStatus"`)
	for _, code := range []string{QualPaid, QualOrganic, QualQualifiedChat, QualQualifiedCall} {
		if !strings.Contains(sql, "'"+code+"'") {
			t.Fatalf("SQL IN list missing %s: %s", code, sql)
		}
	}
}
