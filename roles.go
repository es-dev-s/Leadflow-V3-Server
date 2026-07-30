package main

import "strings"

// Role constants match existing LeadFlow User.role values.
const (
	RoleSuperadmin      = "SUPERADMIN"
	RoleAnalystTeamLead = "ANALYST_TEAM_LEAD"
	RoleLeadAnalyst     = "LEAD_ANALYST"
	RoleMainTeamLead    = "MAIN_TEAM_LEAD"
	RoleSalesExecutive  = "SALES_EXECUTIVE"
	RoleSupport         = "SUPPORT"
)

var allRoles = []string{
	RoleSuperadmin,
	RoleAnalystTeamLead,
	RoleLeadAnalyst,
	RoleMainTeamLead,
	RoleSalesExecutive,
	RoleSupport,
}

var roleLabels = map[string]string{
	RoleSuperadmin:      "Superadmin",
	RoleAnalystTeamLead: "Analyst Team Lead",
	RoleLeadAnalyst:     "Lead Analyst",
	RoleMainTeamLead:    "Main Team Lead",
	RoleSalesExecutive:  "Sales Executive",
	RoleSupport:         "Support",
}

// Roles an Analyst Team Lead may create / edit / delete.
var analystTeamLeadManagedRoles = []string{
	RoleLeadAnalyst,
	RoleMainTeamLead,
	RoleSalesExecutive,
}

func isValidRole(role string) bool {
	_, ok := roleLabels[role]
	return ok
}

func roleLabel(role string) string {
	if label, ok := roleLabels[role]; ok {
		return label
	}
	return role
}

// canManageUsers — who may create / edit / delete within their role scope.
func canManageUsers(role string) bool {
	return role == RoleSuperadmin || role == RoleAnalystTeamLead || isMainTeamLead(role)
}

// canViewUsers — who may open the Users page (read).
func canViewUsers(role string) bool {
	return canManageUsers(role)
}

func canCreateUsers(role string) bool {
	return canManageUsers(role)
}

// canMutateLeads — edit / assign / delete leads (qualification has its own gate).
// Analyst Team Lead is released the same lead-ops surface as Superadmin.
func canMutateLeads(role string) bool {
	switch role {
	case RoleSuperadmin, RoleAnalystTeamLead, RoleLeadAnalyst, RoleMainTeamLead, RoleSalesExecutive:
		return true
	default:
		return false
	}
}

// canChangeQualification — who may set / change lead qualification status.
// Main Team Leads assign within their team; qualification stays with analysts.
// Sales Executives update sales outcome only — not qualification.
func canChangeQualification(role string) bool {
	switch role {
	case RoleSuperadmin, RoleAnalystTeamLead, RoleLeadAnalyst:
		return true
	default:
		return false
	}
}

// canCreateLeads — who may create new leads (SEs work assigned inventory only).
func canCreateLeads(role string) bool {
	return canMutateLeads(role) && !isSalesExecutive(role)
}

// canEditLeadProfile — full lead profile edit (contact, source, notes, etc.).
// Main Team Leads assign within the team; they do not edit lead profile data.
// Sales Executives update sales outcome only.
func canEditLeadProfile(role string) bool {
	switch role {
	case RoleSuperadmin, RoleAnalystTeamLead, RoleLeadAnalyst:
		return true
	default:
		return false
	}
}

// canDeleteLeads — bulk/single lead deletion (Superadmin and ATL only).
func canDeleteLeads(role string) bool {
	switch role {
	case RoleSuperadmin, RoleAnalystTeamLead:
		return true
	default:
		return false
	}
}

// canUpdateSalesOutcome — SE outcome fields (stage, payment, revenue, SE notes).
func canUpdateSalesOutcome(role string) bool {
	switch role {
	case RoleSuperadmin, RoleAnalystTeamLead, RoleSalesExecutive:
		return true
	default:
		return false
	}
}

// canMarkNotAppropriate — only Sales Executives may flag leads as not appropriate.
func canMarkNotAppropriate(role string) bool {
	return isSalesExecutive(role)
}

// canAssignToTeamLeads — who may assign leads to team-lead targets.
// Main Team Leads may only assign to members (sales executives) on their team.
func canAssignToTeamLeads(role string) bool {
	return canMutateLeads(role) && !isSalesExecutive(role) && !isMainTeamLead(role)
}

func isSupport(role string) bool {
	return role == RoleSupport
}

// canViewLeadData — who may list/read leads, pipeline, transfers, and lead analytics.
// Support has no lead-data surface (empty dashboard only).
func canViewLeadData(role string) bool {
	return !isSupport(role) && isValidRole(role)
}

func isLeadAnalyst(role string) bool {
	return role == RoleLeadAnalyst
}

func isMainTeamLead(role string) bool {
	return role == RoleMainTeamLead
}

func isSalesExecutive(role string) bool {
	return role == RoleSalesExecutive
}

// leadDataOwnerID — when non-empty, this actor may only see/mutate leads they created.
func leadDataOwnerID(role, userID string) string {
	if isLeadAnalyst(role) {
		return strings.TrimSpace(userID)
	}
	return ""
}

// leadSalesExecScopeID — when non-empty, this actor may only see leads assigned to them.
func leadSalesExecScopeID(role, userID string) string {
	if isSalesExecutive(role) {
		return strings.TrimSpace(userID)
	}
	return ""
}

// leadTeamScopeID — when non-empty, this actor may only see leads / users on this team.
func leadTeamScopeID(role string, teamID *string) string {
	if !isMainTeamLead(role) {
		return ""
	}
	if teamID == nil {
		return ""
	}
	return strings.TrimSpace(*teamID)
}

// creatableRoles — roles the actor may assign when creating/updating users.
func creatableRoles(actorRole string) []string {
	switch actorRole {
	case RoleSuperadmin:
		out := make([]string, len(allRoles))
		copy(out, allRoles)
		return out
	case RoleAnalystTeamLead:
		out := make([]string, len(analystTeamLeadManagedRoles))
		copy(out, analystTeamLeadManagedRoles)
		return out
	case RoleMainTeamLead:
		return []string{RoleSalesExecutive}
	default:
		return nil
	}
}

// visibleUserRoles — roles the actor may list on the Users page.
func visibleUserRoles(actorRole string) []string {
	switch actorRole {
	case RoleSuperadmin:
		return creatableRoles(actorRole)
	case RoleAnalystTeamLead:
		return creatableRoles(actorRole)
	case RoleMainTeamLead:
		return []string{RoleSalesExecutive}
	default:
		return nil
	}
}

func canCreateRole(actorRole, targetRole string) bool {
	for _, role := range creatableRoles(actorRole) {
		if role == targetRole {
			return true
		}
	}
	return false
}

// canActOnUser — whether actor may edit/delete a user with targetRole (team checks are separate).
func canActOnUser(actorRole, targetRole string) bool {
	if actorRole == RoleSuperadmin {
		return true
	}
	return canCreateRole(actorRole, targetRole)
}

// sameTeamID reports whether both IDs refer to the same team.
func sameTeamID(a, b *string) bool {
	if a == nil || b == nil {
		return false
	}
	left := strings.TrimSpace(*a)
	right := strings.TrimSpace(*b)
	return left != "" && left == right
}

type RoleInfo struct {
	Value string `json:"value"`
	Label string `json:"label"`
}

func listRoles() []RoleInfo {
	return rolesToInfo(allRoles)
}

func listCreatableRoles(actorRole string) []RoleInfo {
	return rolesToInfo(creatableRoles(actorRole))
}

func rolesToInfo(roles []string) []RoleInfo {
	out := make([]RoleInfo, 0, len(roles))
	for _, r := range roles {
		out = append(out, RoleInfo{Value: r, Label: roleLabel(r)})
	}
	return out
}

func roleSet(roles []string) map[string]struct{} {
	out := make(map[string]struct{}, len(roles))
	for _, r := range roles {
		out[r] = struct{}{}
	}
	return out
}
