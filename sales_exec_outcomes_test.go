package main

import (
	"strings"
	"testing"
)

func TestCompleteSalesExecOutcomePartitionsAssigned(t *testing.T) {
	item := SalesExecOutcome{
		Assigned:     49,
		WithTeamLead: 0,
		WithRep:      0,
		InProgress:   21,
		Won:          9,
		Lost:         19,
	}
	completeSalesExecOutcome(&item)
	if item.Other != 0 {
		t.Fatalf("expected other=0, got %d", item.Other)
	}
	got := item.WithTeamLead + item.WithRep + item.InProgress + item.Won + item.Lost + item.Other
	if got != item.Assigned {
		t.Fatalf("partition %d != assigned %d", got, item.Assigned)
	}
}

func TestCompleteSalesExecOutcomeCapturesRemainder(t *testing.T) {
	item := SalesExecOutcome{
		Assigned:     100,
		WithTeamLead: 10,
		WithRep:      20,
		InProgress:   30,
		Won:          5,
		Lost:         5,
	}
	completeSalesExecOutcome(&item)
	if item.Other != 30 {
		t.Fatalf("expected other=30 pre-sales remainder, got %d", item.Other)
	}
}

func TestLeadScopeWhereAssignedToSalesExecExcludesUnassigned(t *testing.T) {
	sql, _ := leadScopeWhereAssignedToSalesExec(LeadListParams{})
	if !strings.Contains(sql, `l."assignedSalesExecId" IS NOT NULL`) {
		t.Fatalf("SE outcomes must exclude unassigned leads: %s", sql)
	}
}

func TestLeadScopeWhereAssignedToTeamExcludesUnassigned(t *testing.T) {
	sql, _ := leadScopeWhereAssignedToTeam(LeadListParams{})
	if !strings.Contains(sql, `l."teamId" IS NOT NULL`) {
		t.Fatalf("team mix must exclude unassigned leads: %s", sql)
	}
}

func TestSalesExecOutcomeSQLCoversPipelineStages(t *testing.T) {
	for _, stage := range []string{
		"WITH_TEAM_LEAD",
		"WITH_EXECUTIVE",
		"NOT_CONNECTED",
		"IN_NEGOTIATION",
		"NO_RESPONSE_FROM_CLIENT",
		"CLOSED_WON",
		"CLOSED_LOST",
	} {
		if !strings.Contains(salesExecOutcomeCountSQL, stage) {
			t.Fatalf("count SQL missing stage %s", stage)
		}
	}
}
