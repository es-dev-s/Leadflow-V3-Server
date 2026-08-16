package main

import (
	"net/http"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestLoginLimiterConcurrentDistinctKeys(t *testing.T) {
	limiter := newLoginLimiter(12, time.Minute)
	const workers = 100
	var allowed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		i := i
		go func() {
			defer wg.Done()
			key := "127.0.0.1|user" + string(rune('a'+i%26)) + string(rune('0'+i/26))
			if limiter.allow(key) {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != workers {
		t.Fatalf("expected all %d distinct keys allowed, got %d", workers, got)
	}
}

func TestLoginLimiterConcurrentSameKeyRespectsLimit(t *testing.T) {
	limiter := newLoginLimiter(5, time.Minute)
	const workers = 40
	var allowed atomic.Int64
	var wg sync.WaitGroup
	wg.Add(workers)
	for i := 0; i < workers; i++ {
		go func() {
			defer wg.Done()
			if limiter.allow("127.0.0.1|same@demo.local") {
				allowed.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := allowed.Load(); got != 5 {
		t.Fatalf("expected limit 5, got %d", got)
	}
}

func TestTokenServiceConcurrentIssueParseNoMix(t *testing.T) {
	tokens := NewTokenService("test-secret-for-concurrency", time.Hour)
	users := []AuthUser{
		{ID: "id-a", Email: "a@demo.local", Name: "A", Role: RoleSalesExecutive},
		{ID: "id-b", Email: "b@demo.local", Name: "B", Role: RoleMainTeamLead},
		{ID: "id-c", Email: "c@demo.local", Name: "C", Role: RoleSuperadmin},
		{ID: "id-d", Email: "d@demo.local", Name: "D", Role: RoleSupport},
		{ID: "id-e", Email: "e@demo.local", Name: "E", Role: RoleLeadAnalyst},
	}

	const rounds = 40
	var wg sync.WaitGroup
	errCh := make(chan error, len(users)*rounds)

	for _, user := range users {
		user := user
		for r := 0; r < rounds; r++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				token, _, err := tokens.Issue(user)
				if err != nil {
					errCh <- err
					return
				}
				parsed, err := tokens.Parse(token)
				if err != nil {
					errCh <- err
					return
				}
				if parsed.ID != user.ID || parsed.Email != user.Email || parsed.Role != user.Role {
					errCh <- errIdentityMix(user, *parsed)
				}
			}()
		}
	}
	wg.Wait()
	close(errCh)
	for err := range errCh {
		t.Fatal(err)
	}
}

type identityMixError struct {
	want AuthUser
	got  AuthUser
}

func errIdentityMix(want, got AuthUser) error {
	return identityMixError{want: want, got: got}
}

func (e identityMixError) Error() string {
	return "token identity mix: want " + e.want.Email + "/" + e.want.Role +
		" got " + e.got.Email + "/" + e.got.Role
}

func TestClientIPIgnoresSpoofedForwardedHeadersByDefault(t *testing.T) {
	t.Setenv("TRUST_PROXY", "")
	req := &http.Request{
		RemoteAddr: "203.0.113.10:54321",
		Header: http.Header{
			"X-Forwarded-For": []string{"1.2.3.4"},
			"X-Real-Ip":       []string{"5.6.7.8"},
		},
	}
	if got := clientIP(req); got != "203.0.113.10" {
		t.Fatalf("expected remote addr IP, got %q", got)
	}
}

func TestClientIPTrustsProxyWhenEnabled(t *testing.T) {
	t.Setenv("TRUST_PROXY", "true")
	req := &http.Request{
		RemoteAddr: "203.0.113.10:54321",
		Header: http.Header{
			"X-Forwarded-For": []string{"1.2.3.4, 9.9.9.9"},
		},
	}
	if got := clientIP(req); got != "1.2.3.4" {
		t.Fatalf("expected forwarded IP, got %q", got)
	}
}

func TestCanCreateUsersRBAC(t *testing.T) {
	if !canCreateUsers(RoleSuperadmin) {
		t.Fatal("superadmin should create users")
	}
	if !canCreateUsers(RoleAnalystTeamLead) {
		t.Fatal("analyst team lead should create users")
	}
	for _, role := range []string{
		RoleLeadAnalyst,
		RoleSalesExecutive,
		RoleSupport,
	} {
		if canCreateUsers(role) {
			t.Fatalf("%s must not create users", role)
		}
	}
	if !canCreateUsers(RoleMainTeamLead) {
		t.Fatal("main team lead should manage team sales executives")
	}
	if !canCreateRole(RoleMainTeamLead, RoleSalesExecutive) {
		t.Fatal("MTL should create sales executives")
	}
	if canCreateRole(RoleMainTeamLead, RoleSuperadmin) {
		t.Fatal("MTL must not create superadmins")
	}

	if !canCreateRole(RoleAnalystTeamLead, RoleLeadAnalyst) {
		t.Fatal("ATL should create lead analysts")
	}
	if !canCreateRole(RoleAnalystTeamLead, RoleMainTeamLead) {
		t.Fatal("ATL should create main team leads")
	}
	if canCreateRole(RoleAnalystTeamLead, RoleSuperadmin) {
		t.Fatal("ATL must not create superadmins")
	}
	if !canCreateRole(RoleAnalystTeamLead, RoleSalesExecutive) {
		t.Fatal("ATL should create sales executives")
	}
	if !canActOnUser(RoleAnalystTeamLead, RoleSalesExecutive) {
		t.Fatal("ATL should manage sales executives")
	}
	if !canActOnUser(RoleAnalystTeamLead, RoleLeadAnalyst) {
		t.Fatal("ATL should manage lead analysts")
	}
	if canActOnUser(RoleAnalystTeamLead, RoleSuperadmin) {
		t.Fatal("ATL must not manage superadmins")
	}

	if !canMutateLeads(RoleAnalystTeamLead) {
		t.Fatal("ATL should mutate leads")
	}
	if !canMutateLeads(RoleSuperadmin) {
		t.Fatal("superadmin should mutate leads")
	}
	if !canMutateLeads(RoleSalesExecutive) {
		t.Fatal("sales executive should mutate assigned leads")
	}
	if canChangeQualification(RoleMainTeamLead) {
		t.Fatal("main team lead must not change qualification")
	}
	if canChangeQualification(RoleSalesExecutive) {
		t.Fatal("sales executive must not change qualification")
	}
	if !canChangeQualification(RoleLeadAnalyst) {
		t.Fatal("lead analyst should change qualification")
	}
	if canEditLeadProfile(RoleSalesExecutive) || canDeleteLeads(RoleSalesExecutive) {
		t.Fatal("sales executive must not edit profile or delete leads")
	}
	if canEditLeadProfile(RoleMainTeamLead) || canDeleteLeads(RoleMainTeamLead) {
		t.Fatal("main team lead must not edit profile or delete leads")
	}
	if canCreateLeads(RoleMainTeamLead) || canCreateLeads(RoleSalesExecutive) {
		t.Fatal("main team lead and sales executive must not create leads")
	}
	if !canCreateLeads(RoleSuperadmin) || !canCreateLeads(RoleAnalystTeamLead) || !canCreateLeads(RoleLeadAnalyst) {
		t.Fatal("superadmin, ATL, and lead analyst should create leads")
	}
	if canEditLeadProfile(RoleLeadAnalyst) || canDeleteLeads(RoleLeadAnalyst) {
		t.Fatal("lead analyst must not edit profile or delete leads")
	}
	if !canEditLeadProfile(RoleSuperadmin) || !canEditLeadProfile(RoleAnalystTeamLead) {
		t.Fatal("superadmin and ATL should edit lead profiles")
	}
	if !canDeleteLeads(RoleSuperadmin) || !canDeleteLeads(RoleAnalystTeamLead) {
		t.Fatal("superadmin and ATL should delete leads")
	}
	if !canUpdateSalesOutcome(RoleSalesExecutive) {
		t.Fatal("sales executive should update sales outcome")
	}
	if !canMarkNotAppropriate(RoleSalesExecutive) {
		t.Fatal("sales executive should mark leads not appropriate")
	}
	if canMarkNotAppropriate(RoleSuperadmin) || canMarkNotAppropriate(RoleAnalystTeamLead) {
		t.Fatal("only sales executives may mark leads not appropriate")
	}
	if canAssignToTeamLeads(RoleMainTeamLead) {
		t.Fatal("main team lead must not assign to team leads")
	}
	if !canAssignToTeamLeads(RoleSuperadmin) {
		t.Fatal("superadmin should assign to team leads")
	}
	if leadDataOwnerID(RoleLeadAnalyst, "u-1") != "u-1" {
		t.Fatal("lead analyst should be creator-scoped")
	}
	if leadDataOwnerID(RoleSuperadmin, "u-1") != "" {
		t.Fatal("superadmin must not be creator-scoped")
	}
	if leadSalesExecScopeID(RoleSalesExecutive, "se-1") != "se-1" {
		t.Fatal("sales executive should be assignee-scoped")
	}
	if leadSalesExecScopeID(RoleSuperadmin, "se-1") != "" {
		t.Fatal("superadmin must not be assignee-scoped")
	}

	team := "team-1"
	if leadTeamScopeID(RoleMainTeamLead, &team) != "team-1" {
		t.Fatal("main team lead should be team-scoped")
	}
	if leadTeamScopeID(RoleSuperadmin, &team) != "" {
		t.Fatal("superadmin must not be team-scoped")
	}
	if !canViewUsers(RoleMainTeamLead) {
		t.Fatal("main team lead should view users")
	}
	if !canManageUsers(RoleMainTeamLead) {
		t.Fatal("main team lead should manage team sales executives")
	}
	vis := visibleUserRoles(RoleMainTeamLead)
	if len(vis) != 1 || vis[0] != RoleSalesExecutive {
		t.Fatalf("main team lead should only see sales executives, got %v", vis)
	}
}
