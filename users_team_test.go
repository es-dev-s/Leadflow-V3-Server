package main

import (
	"errors"
	"testing"
)

func TestResolveAnalystTeamNameRequiresATLTeam(t *testing.T) {
	existing := &UserRecord{}
	if _, err := resolveAnalystTeamName(existing, RoleAnalystTeamLead, nil); !errors.Is(err, errTeamNameRequired) {
		t.Fatalf("ATL without a team must fail, got %v", err)
	}

	name := "  Qualification Pod A  "
	got, err := resolveAnalystTeamName(existing, RoleAnalystTeamLead, &name)
	if err != nil {
		t.Fatal(err)
	}
	if got == nil || *got != "Qualification Pod A" {
		t.Fatalf("got %#v", got)
	}
}

func TestResolveAnalystTeamNameLeadAnalystOptional(t *testing.T) {
	existing := &UserRecord{}
	empty := "  "
	got, err := resolveAnalystTeamName(existing, RoleLeadAnalyst, &empty)
	if err != nil {
		t.Fatal(err)
	}
	if got != nil {
		t.Fatalf("empty analyst team should clear, got %#v", got)
	}
}
