package main

import (
	"bytes"
	"strings"
	"testing"
	"time"
)

func TestBuildLeadsExportPDFEmpty(t *testing.T) {
	pdf, err := buildLeadsExportPDF(LeadExportResult{
		MatchTotal: 0,
		Filter:     "all",
		Sort:       "newest",
	}, leadExportMeta{
		ExportedBy: "Alex Sales",
		RoleLabel:  "Sales Executive",
		ScopeNote:  exportScopeNote(RoleSalesExecutive),
		Filters:    []string{"All leads in your current access scope"},
		Generated:  time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("expected PDF magic, got %q", pdf[:min(8, len(pdf))])
	}
	if len(pdf) < 400 {
		t.Fatalf("empty-state PDF unexpectedly small: %d bytes", len(pdf))
	}
}

func TestBuildLeadsExportPDFRows(t *testing.T) {
	pdf, err := buildLeadsExportPDF(LeadExportResult{
		Rows: []LeadExportRow{
			{
				Name: "Priya Sharma", Phone: "+61 400 000 001", Email: "priya@example.com",
				Location: "Sydney, Australia", Source: "Facebook", Portal: "CDR",
				Qualification: "Qualified", SalesStatus: "In Negotiation",
				Added: "2026-08-01", Deal: "AUD 2400",
			},
			{
				Name: "Sam Lee", Phone: "+61 400 000 002", Email: "sam@example.com",
				Location: "Melbourne, Australia", Source: "Website", Portal: "PTE Hub",
				Qualification: "Not Qualified", SalesStatus: "With executive",
				Added: "2026-08-10", Deal: "—",
			},
		},
		MatchTotal: 2,
		Filter:     "qualified",
		Sort:       "newest",
	}, leadExportMeta{
		ExportedBy: "Alex Sales",
		RoleLabel:  "Sales Executive",
		ScopeNote:  exportScopeNote(RoleSalesExecutive),
		Filters:    []string{"Qualified"},
		Generated:  time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.HasPrefix(pdf, []byte("%PDF")) {
		t.Fatalf("expected PDF magic, got %q", pdf[:min(8, len(pdf))])
	}
	if len(pdf) < 800 {
		t.Fatalf("PDF unexpectedly small: %d bytes", len(pdf))
	}
}

func TestFormatInt64(t *testing.T) {
	if got := formatInt64(133007); got != "133,007" {
		t.Fatalf("got %q", got)
	}
	if got := formatInt64(12); got != "12" {
		t.Fatalf("got %q", got)
	}
}

func TestDescribeLeadExportFilters(t *testing.T) {
	none := describeLeadExportFilters(LeadListParams{Filter: "all"})
	if len(none) != 1 || !strings.Contains(none[0], "All leads") {
		t.Fatalf("unfiltered: %v", none)
	}

	chips := describeLeadExportFilters(LeadListParams{
		Filter:    "qualified",
		Query:     "priya",
		Country:   "Australia",
		Stage:     "IN_NEGOTIATION",
		AddedFrom: "2026-08-01",
		AddedTo:   "2026-08-16",
	})
	joined := strings.Join(chips, " | ")
	for _, want := range []string{"Qualified", "Search: priya", "Country: Australia", "In Negotiation", "2026-08-01"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in %q", want, joined)
		}
	}
}

func TestLeadExportFilename(t *testing.T) {
	when := time.Date(2026, 8, 16, 8, 0, 0, 0, time.UTC)
	name := leadExportFilename(LeadListParams{Filter: "all"}, when)
	if !strings.HasPrefix(name, "leadflow-leads-") || !strings.HasSuffix(name, ".pdf") {
		t.Fatalf("plain name: %s", name)
	}
	filtered := leadExportFilename(LeadListParams{Filter: "qualified", Country: "Australia"}, when)
	if !strings.Contains(filtered, "qualified") || !strings.Contains(filtered, "filtered") {
		t.Fatalf("filtered name: %s", filtered)
	}
}

func TestExportScopeNote(t *testing.T) {
	if !strings.Contains(exportScopeNote(RoleSalesExecutive), "assigned") {
		t.Fatal("SE scope should mention assigned leads")
	}
}
