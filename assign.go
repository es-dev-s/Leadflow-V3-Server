package main

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

type AssignableUser struct {
	ID        string  `json:"id"`
	Name      string  `json:"name"`
	Email     string  `json:"email"`
	Role      string  `json:"role"`
	RoleLabel string  `json:"roleLabel"`
	TeamID    *string `json:"teamId"`
	TeamName  *string `json:"teamName"`
}

type AssignLeadInput struct {
	LeadIDs           []string
	AssigneeID        string
	Kind              string // team-lead | member
	ActorID           string
	CreatedByID       string // when set, only allow leads created by this user
	TeamID            string // when set, only allow leads / assignees on this team
	SalesExecID       string // when set, only allow leads assigned to this SE
	AnalystTeamLeadID string
	AnalystTeamName   string
}

type LeadAssignmentResult struct {
	LeadID         string `json:"leadId"`
	Team           string `json:"team"`
	SalesExecutive string `json:"salesExecutive"`
	Handoff        string `json:"handoff"`
}

type AssignLeadsResult struct {
	Updated     int                    `json:"updated"`
	Assignments []LeadAssignmentResult `json:"assignments"`
}

func (s *LeadStore) ListAssignableUsers(ctx context.Context, kind, teamID string) ([]AssignableUser, error) {
	var roles []string
	switch kind {
	case "team-leads":
		roles = []string{RoleMainTeamLead, RoleAnalystTeamLead}
	case "members":
		roles = []string{RoleSalesExecutive}
	default:
		return nil, fmt.Errorf("invalid kind")
	}

	args := []any{roles}
	sql := `
		SELECT
			u.id,
			u.name,
			u.email,
			u.role,
			u."teamId",
			t.name
		FROM "User" u
		LEFT JOIN "Team" t ON t.id = u."teamId"
		WHERE u.role = ANY($1)
		  AND COALESCE(u."isActive", TRUE) = TRUE`
	if scoped := strings.TrimSpace(teamID); scoped != "" {
		args = append(args, scoped)
		sql += fmt.Sprintf(` AND u."teamId" = $%d`, len(args))
	}
	sql += ` ORDER BY COALESCE(t.name, 'zzz'), u.name ASC`

	rows, err := s.pool.Query(ctx, sql, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AssignableUser, 0, 64)
	for rows.Next() {
		var item AssignableUser
		if err := rows.Scan(
			&item.ID,
			&item.Name,
			&item.Email,
			&item.Role,
			&item.TeamID,
			&item.TeamName,
		); err != nil {
			return nil, err
		}
		if label, ok := roleLabels[item.Role]; ok {
			item.RoleLabel = label
		} else {
			item.RoleLabel = item.Role
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

const maxBulkLeadIDs = 100

func (s *LeadStore) AssignLeads(ctx context.Context, in AssignLeadInput) (AssignLeadsResult, error) {
	empty := AssignLeadsResult{Assignments: []LeadAssignmentResult{}}
	if len(in.LeadIDs) == 0 {
		return empty, fmt.Errorf("no leads selected")
	}
	if strings.TrimSpace(in.AssigneeID) == "" {
		return empty, fmt.Errorf("assignee is required")
	}
	if in.Kind != "team-lead" && in.Kind != "member" {
		return empty, fmt.Errorf("invalid assign kind")
	}

	cleanIDs := make([]string, 0, len(in.LeadIDs))
	seen := map[string]struct{}{}
	for _, id := range in.LeadIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		cleanIDs = append(cleanIDs, id)
	}
	if len(cleanIDs) == 0 {
		return empty, fmt.Errorf("no leads selected")
	}
	if len(cleanIDs) > maxBulkLeadIDs {
		return empty, fmt.Errorf("too many leads selected (max %d)", maxBulkLeadIDs)
	}

	actorID := strings.TrimSpace(in.ActorID)
	if actorID == "" {
		id, err := s.defaultCreatorID(ctx)
		if err != nil {
			return empty, fmt.Errorf("no actor available: %w", err)
		}
		actorID = id
	}

	var (
		assigneeName  string
		assigneeEmail string
		assigneeRole  string
		teamID        *string
		teamName      *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT u.name, u.email, u.role, u."teamId", t.name
		FROM "User" u
		LEFT JOIN "Team" t ON t.id = u."teamId"
		WHERE u.id = $1
		LIMIT 1`, in.AssigneeID).Scan(
		&assigneeName, &assigneeEmail, &assigneeRole, &teamID, &teamName,
	)
	if err != nil {
		return empty, fmt.Errorf("assignee not found")
	}

	if scope := strings.TrimSpace(in.TeamID); scope != "" {
		if teamID == nil || strings.TrimSpace(*teamID) != scope {
			return empty, fmt.Errorf("assignee is not on your team")
		}
	}

	if in.Kind == "team-lead" {
		if assigneeRole != RoleMainTeamLead && assigneeRole != RoleAnalystTeamLead {
			return empty, fmt.Errorf("assignee is not a team lead")
		}
	} else {
		if assigneeRole != RoleSalesExecutive {
			return empty, fmt.Errorf("assignee is not a team member")
		}
	}

	// For member assigns, resolve the team's main team lead when possible.
	var mainTeamLeadID *string
	var mainTeamLeadName *string
	if in.Kind == "member" && teamID != nil {
		var mtlID, mtlName string
		err := s.pool.QueryRow(ctx, `
			SELECT id, name FROM "User"
			WHERE role = $1 AND "teamId" = $2
			ORDER BY "createdAt" ASC
			LIMIT 1`, RoleMainTeamLead, *teamID).Scan(&mtlID, &mtlName)
		if err == nil {
			mainTeamLeadID = &mtlID
			mainTeamLeadName = &mtlName
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer tx.Rollback(ctx)

	lockQuery := `
		SELECT id, "qualificationStatus"
		FROM "Lead"
		WHERE id = ANY($1)`
	lockArgs := []any{cleanIDs}
	argN := 1
	if owner := strings.TrimSpace(in.CreatedByID); owner != "" {
		argN++
		lockQuery += fmt.Sprintf(` AND "createdById" = $%d`, argN)
		lockArgs = append(lockArgs, owner)
	}
	if teamScope := strings.TrimSpace(in.TeamID); teamScope != "" {
		argN++
		lockQuery += fmt.Sprintf(` AND "teamId" = $%d`, argN)
		lockArgs = append(lockArgs, teamScope)
	}
	if seScope := strings.TrimSpace(in.SalesExecID); seScope != "" {
		argN++
		lockQuery += fmt.Sprintf(` AND "assignedSalesExecId" = $%d`, argN)
		lockArgs = append(lockArgs, seScope)
	}
	if sql := analystTeamLeadScopeSQL("", &lockArgs, in.AnalystTeamLeadID, in.AnalystTeamName); sql != "" {
		lockQuery += ` AND ` + sql
	}
	lockQuery += ` FOR UPDATE`

	rows, err := tx.Query(ctx, lockQuery, lockArgs...)
	if err != nil {
		return empty, err
	}
	found := map[string]string{}
	for rows.Next() {
		var id, status string
		if err := rows.Scan(&id, &status); err != nil {
			rows.Close()
			return empty, err
		}
		found[id] = status
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return empty, err
	}
	if len(found) != len(cleanIDs) {
		return empty, fmt.Errorf("one or more leads were not found")
	}
	for _, id := range cleanIDs {
		if !isAssignableQualification(found[id]) {
			return empty, fmt.Errorf("only qualified leads can be assigned to a team or members")
		}
	}
	in.LeadIDs = cleanIDs

	now := time.Now().UTC()
	assignments := make([]LeadAssignmentResult, 0, len(in.LeadIDs))

	teamLabel := "Unassigned team"
	if teamName != nil && strings.TrimSpace(*teamName) != "" {
		teamLabel = strings.TrimSpace(*teamName)
	}

	for _, leadID := range in.LeadIDs {
		var action, detail string
		var salesExecDisplay string
		if in.Kind == "team-lead" {
			tag, err := tx.Exec(ctx, `
				UPDATE "Lead"
				SET
					"assignedMainTeamLeadId" = $2,
					"teamId" = $3,
					"assignedSalesExecId" = NULL,
					"execAssignedAt" = NULL,
					"execDeadlineAt" = NULL,
					"salesStage" = 'WITH_TEAM_LEAD',
					"updatedAt" = $4
				WHERE id = $1
				  AND "qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CHAT', 'QUALIFIED_CALL', 'PAID', 'ORGANIC')`,
				leadID, in.AssigneeID, teamID, now)
			if err != nil {
				return empty, err
			}
			if tag.RowsAffected() == 0 {
				return empty, fmt.Errorf("only qualified leads can be assigned to a team or members")
			}
			action = "ROUTED_TO_MAIN_TEAM"
			detail = fmt.Sprintf("Main team lead: %s · Team: %s", assigneeName, teamLabel)
			salesExecDisplay = "—"
		} else {
			tag, err := tx.Exec(ctx, `
				UPDATE "Lead"
				SET
					"assignedSalesExecId" = $2,
					"teamId" = $3,
					"assignedMainTeamLeadId" = COALESCE($4, "assignedMainTeamLeadId"),
					"execAssignedAt" = $5,
					"salesStage" = CASE
						WHEN "salesStage" IN (
							'PRE_SALES',
							'WITH_TEAM_LEAD',
							'NOT_CONNECTED',
							''
						) OR "salesStage" IS NULL
						THEN 'WITH_EXECUTIVE'
						ELSE "salesStage"
					END,
					"updatedAt" = $5
				WHERE id = $1
				  AND "qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CHAT', 'QUALIFIED_CALL', 'PAID', 'ORGANIC')`,
				leadID, in.AssigneeID, teamID, mainTeamLeadID, now,
			)
			if err != nil {
				return empty, err
			}
			if tag.RowsAffected() == 0 {
				return empty, fmt.Errorf("only qualified leads can be assigned to a team or members")
			}
			action = "DIRECT_ASSIGNED_TO_EXECUTIVE_BY_ATL"
			mtlLabel := "—"
			if mainTeamLeadName != nil && strings.TrimSpace(*mainTeamLeadName) != "" {
				mtlLabel = *mainTeamLeadName
			}
			detail = fmt.Sprintf(
				"Main team lead: %s · Team: %s · Sales executive: %s (%s)",
				mtlLabel, teamLabel, assigneeName, assigneeEmail,
			)
			salesExecDisplay = assigneeName
		}

		_, err = tx.Exec(ctx, `
			INSERT INTO "LeadHandoffLog" (id, "createdAt", "leadId", action, "actorId", detail)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			uuid.NewString(), now, leadID, action, actorID, detail,
		)
		if err != nil {
			return empty, err
		}

		assignments = append(assignments, LeadAssignmentResult{
			LeadID:         leadID,
			Team:           teamLabel,
			SalesExecutive: salesExecDisplay,
			Handoff:        formatHandoff(&action, &detail),
		})
	}

	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}
	return AssignLeadsResult{
		Updated:     len(assignments),
		Assignments: assignments,
	}, nil
}

func (s *LeadStore) LeadNameBriefs(ctx context.Context, leadIDs []string) (map[string]string, error) {
	clean := make([]string, 0, len(leadIDs))
	seen := map[string]struct{}{}
	for _, id := range leadIDs {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		clean = append(clean, id)
	}
	out := map[string]string{}
	if len(clean) == 0 {
		return out, nil
	}
	rows, err := s.pool.Query(ctx, `
		SELECT id, "leadName" FROM "Lead" WHERE id = ANY($1)`, clean)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id, name string
		if err := rows.Scan(&id, &name); err != nil {
			return nil, err
		}
		out[id] = name
	}
	return out, rows.Err()
}

func (s *LeadStore) DeleteLeads(ctx context.Context, leadIDs []string, createdByID, teamID, salesExecID, analystTeamLeadID, analystTeamName string) (int, error) {
	if len(leadIDs) == 0 {
		return 0, fmt.Errorf("no leads selected")
	}
	clean := make([]string, 0, len(leadIDs))
	for _, id := range leadIDs {
		id = strings.TrimSpace(id)
		if id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return 0, fmt.Errorf("no leads selected")
	}
	if len(clean) > maxBulkLeadIDs {
		return 0, fmt.Errorf("too many leads selected (max %d)", maxBulkLeadIDs)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return 0, err
	}
	defer tx.Rollback(ctx)

	owner := strings.TrimSpace(createdByID)
	teamScope := strings.TrimSpace(teamID)
	seScope := strings.TrimSpace(salesExecID)
	deleteSQL := `DELETE FROM "Lead" WHERE id = ANY($1)`
	deleteArgs := []any{clean}
	argN := 1
	if owner != "" {
		argN++
		deleteSQL += fmt.Sprintf(` AND "createdById" = $%d`, argN)
		deleteArgs = append(deleteArgs, owner)
	}
	if teamScope != "" {
		argN++
		deleteSQL += fmt.Sprintf(` AND "teamId" = $%d`, argN)
		deleteArgs = append(deleteArgs, teamScope)
	}
	if seScope != "" {
		argN++
		deleteSQL += fmt.Sprintf(` AND "assignedSalesExecId" = $%d`, argN)
		deleteArgs = append(deleteArgs, seScope)
	}
	if sql := analystTeamLeadScopeSQL("", &deleteArgs, analystTeamLeadID, analystTeamName); sql != "" {
		deleteSQL += ` AND ` + sql
	}

	// Only remove handoffs for leads that will actually be deleted.
	handoffSQL := `DELETE FROM "LeadHandoffLog" WHERE "leadId" IN (
		SELECT id FROM "Lead" WHERE id = ANY($1)`
	handoffArgs := []any{clean}
	argN = 1
	if owner != "" {
		argN++
		handoffSQL += fmt.Sprintf(` AND "createdById" = $%d`, argN)
		handoffArgs = append(handoffArgs, owner)
	}
	if teamScope != "" {
		argN++
		handoffSQL += fmt.Sprintf(` AND "teamId" = $%d`, argN)
		handoffArgs = append(handoffArgs, teamScope)
	}
	if seScope != "" {
		argN++
		handoffSQL += fmt.Sprintf(` AND "assignedSalesExecId" = $%d`, argN)
		handoffArgs = append(handoffArgs, seScope)
	}
	if sql := analystTeamLeadScopeSQL("", &handoffArgs, analystTeamLeadID, analystTeamName); sql != "" {
		handoffSQL += ` AND ` + sql
	}
	handoffSQL += `)`
	if _, err = tx.Exec(ctx, handoffSQL, handoffArgs...); err != nil {
		return 0, err
	}

	tag, err := tx.Exec(ctx, deleteSQL, deleteArgs...)
	if err != nil {
		return 0, err
	}
	if err := tx.Commit(ctx); err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}
