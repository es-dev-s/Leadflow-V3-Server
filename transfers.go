package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultTransferLimit = 40
	maxTransferLimit     = 100
)

type SalesExecTeamTransferLog struct {
	ID                string    `json:"id"`
	SalesExecID       string    `json:"salesExecId"`
	SalesExecName     string    `json:"salesExecName"`
	FromTeamID        *string   `json:"fromTeamId"`
	FromTeamName      *string   `json:"fromTeamName"`
	ToTeamID          string    `json:"toTeamId"`
	ToTeamName        string    `json:"toTeamName"`
	TransferredByID   string    `json:"transferredById"`
	TransferredByName string    `json:"transferredByName"`
	CreatedAt         time.Time `json:"createdAt"`
}

type LeadTransferLog struct {
	ID          string    `json:"id"`
	LeadID      string    `json:"leadId"`
	LeadName    string    `json:"leadName"`
	Action      string    `json:"action"`
	ActionLabel string    `json:"actionLabel"`
	ActorID     *string   `json:"actorId"`
	ActorName   *string   `json:"actorName"`
	Detail      *string   `json:"detail"`
	CreatedAt   time.Time `json:"createdAt"`
}

type TransferListResult struct {
	Type       string          `json:"type"`
	Items      any             `json:"items"`
	Total      int64           `json:"total"`
	NextCursor string          `json:"nextCursor,omitempty"`
	HasMore    bool            `json:"hasMore"`
	Limit      int             `json:"limit"`
	Query      string          `json:"query,omitempty"`
	Action     string          `json:"action,omitempty"`
	Totals     TransferTotals  `json:"totals"`
	ActionMix  []ActionMixItem `json:"actionMix,omitempty"`
}

type TransferTotals struct {
	Leads         int64 `json:"leads"`
	SalesExecTeam int64 `json:"salesExecTeam"`
}

type ActionMixItem struct {
	Action string `json:"action"`
	Label  string `json:"label"`
	Count  int    `json:"count"`
}

type transferCursor struct {
	T  string `json:"t"` // RFC3339Nano createdAt
	ID string `json:"i"`
}

var leadTransferActionLabels = map[string]string{
	"DIRECT_ASSIGNED_TO_EXECUTIVE_BY_ATL": "Assigned by ATL",
	"ASSIGNED_TO_EXECUTIVE":               "Assigned to executive",
	"UNASSIGNED_BY_ATL":                   "Unassigned by ATL",
	"ROUTED_TO_MAIN_TEAM":                 "Routed to main team",
}

var leadTransferActions = []string{
	"DIRECT_ASSIGNED_TO_EXECUTIVE_BY_ATL",
	"ASSIGNED_TO_EXECUTIVE",
	"UNASSIGNED_BY_ATL",
	"ROUTED_TO_MAIN_TEAM",
}

type TransferStore struct {
	pool *pgxpool.Pool
}

func NewTransferStore(pool *pgxpool.Pool) *TransferStore {
	return &TransferStore{pool: pool}
}

func clampTransferLimit(limit int) int {
	if limit <= 0 {
		return defaultTransferLimit
	}
	if limit > maxTransferLimit {
		return maxTransferLimit
	}
	return limit
}

func encodeTransferCursor(c transferCursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeTransferCursor(s string) (transferCursor, error) {
	var c transferCursor
	if strings.TrimSpace(s) == "" {
		return c, nil
	}
	raw, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil {
		return c, fmt.Errorf("invalid cursor")
	}
	if err := json.Unmarshal(raw, &c); err != nil {
		return c, fmt.Errorf("invalid cursor")
	}
	return c, nil
}

func (s *TransferStore) Totals(ctx context.Context, createdByID, teamID string) (TransferTotals, error) {
	var out TransferTotals
	owner := strings.TrimSpace(createdByID)
	team := strings.TrimSpace(teamID)

	if team != "" {
		err := s.pool.QueryRow(ctx, `
			SELECT
				(SELECT COUNT(*)::bigint
				 FROM "SalesExecTeamTransfer" s
				 WHERE s."fromTeamId" = $1 OR s."toTeamId" = $1),
				(SELECT COUNT(*)::bigint
				 FROM "LeadHandoffLog" h
				 INNER JOIN "Lead" l ON l.id = h."leadId"
				 WHERE h.action = ANY($2)
				   AND l."teamId" = $1)`, team, leadTransferActions).Scan(
			&out.SalesExecTeam,
			&out.Leads,
		)
		return out, err
	}

	if owner == "" {
		err := s.pool.QueryRow(ctx, `
			SELECT
				(SELECT COUNT(*)::bigint FROM "SalesExecTeamTransfer"),
				(SELECT COUNT(*)::bigint FROM "LeadHandoffLog"
				 WHERE action = ANY($1))`, leadTransferActions).Scan(
			&out.SalesExecTeam,
			&out.Leads,
		)
		return out, err
	}
	// Lead analysts only see handoffs for leads they created; SE team moves stay hidden.
	out.SalesExecTeam = 0
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint
		FROM "LeadHandoffLog" h
		INNER JOIN "Lead" l ON l.id = h."leadId"
		WHERE h.action = ANY($1)
		  AND l."createdById" = $2`, leadTransferActions, owner).Scan(&out.Leads)
	return out, err
}

func (s *TransferStore) LeadActionMix(ctx context.Context, createdByID, teamID string) ([]ActionMixItem, error) {
	owner := strings.TrimSpace(createdByID)
	team := strings.TrimSpace(teamID)
	query := `
		SELECT h.action, COUNT(*)::int
		FROM "LeadHandoffLog" h`
	args := []any{leadTransferActions}
	where := ` WHERE h.action = ANY($1)`
	if team != "" {
		query += `
		INNER JOIN "Lead" l ON l.id = h."leadId"`
		args = append(args, team)
		where += ` AND l."teamId" = $2`
	} else if owner != "" {
		query += `
		INNER JOIN "Lead" l ON l.id = h."leadId"`
		args = append(args, owner)
		where += ` AND l."createdById" = $2`
	}
	query += where + `
		GROUP BY 1
		ORDER BY 2 DESC`
	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]ActionMixItem, 0, len(leadTransferActions))
	for rows.Next() {
		var item ActionMixItem
		if err := rows.Scan(&item.Action, &item.Count); err != nil {
			return nil, err
		}
		if label, ok := leadTransferActionLabels[item.Action]; ok {
			item.Label = label
		} else {
			item.Label = item.Action
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *TransferStore) ListSalesExecTeamTransfers(
	ctx context.Context,
	query string,
	cursorRaw string,
	limit int,
	teamID string,
) ([]SalesExecTeamTransferLog, string, bool, int64, error) {
	limit = clampTransferLimit(limit)
	q := strings.TrimSpace(query)
	team := strings.TrimSpace(teamID)

	countArgs := make([]any, 0, 2)
	countWhere := []string{"TRUE"}
	if team != "" {
		countArgs = append(countArgs, team)
		countWhere = append(countWhere, fmt.Sprintf(
			`(s."fromTeamId" = $%d OR s."toTeamId" = $%d)`, len(countArgs), len(countArgs),
		))
	}
	if q != "" {
		countArgs = append(countArgs, "%"+strings.ToLower(q)+"%")
		n := len(countArgs)
		countWhere = append(countWhere, fmt.Sprintf(`(
			lower(COALESCE(se.name, '')) LIKE $%d
			OR lower(COALESCE(ft.name, '')) LIKE $%d
			OR lower(COALESCE(tt.name, '')) LIKE $%d
			OR lower(COALESCE(tb.name, '')) LIKE $%d
		)`, n, n, n, n))
	}
	countSQL := `
		SELECT COUNT(*)::bigint
		FROM "SalesExecTeamTransfer" s
		LEFT JOIN "User" se ON se.id = s."salesExecId"
		LEFT JOIN "Team" ft ON ft.id = s."fromTeamId"
		LEFT JOIN "Team" tt ON tt.id = s."toTeamId"
		LEFT JOIN "User" tb ON tb.id = s."transferredById"
		WHERE ` + strings.Join(countWhere, " AND ")

	var total int64
	if err := s.pool.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, "", false, 0, err
	}

	args := make([]any, 0, 8)
	where := []string{"TRUE"}
	if team != "" {
		args = append(args, team)
		where = append(where, fmt.Sprintf(
			`(s."fromTeamId" = $%d OR s."toTeamId" = $%d)`, len(args), len(args),
		))
	}
	if q != "" {
		args = append(args, "%"+strings.ToLower(q)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(`(
			lower(COALESCE(se.name, '')) LIKE $%d
			OR lower(COALESCE(ft.name, '')) LIKE $%d
			OR lower(COALESCE(tt.name, '')) LIKE $%d
			OR lower(COALESCE(tb.name, '')) LIKE $%d
		)`, n, n, n, n))
	}

	cur, err := decodeTransferCursor(cursorRaw)
	if err != nil {
		return nil, "", false, 0, err
	}
	if cur.T != "" && cur.ID != "" {
		t, err := time.Parse(time.RFC3339Nano, cur.T)
		if err != nil {
			t, err = time.Parse(time.RFC3339, cur.T)
			if err != nil {
				return nil, "", false, 0, fmt.Errorf("invalid cursor")
			}
		}
		args = append(args, t, cur.ID)
		a, b := len(args)-1, len(args)
		where = append(where, fmt.Sprintf(`(s."createdAt", s.id) < ($%d, $%d)`, a, b))
	}

	whereSQL := strings.Join(where, " AND ")
	args = append(args, limit+1)
	limitPos := len(args)
	listSQL := fmt.Sprintf(`
		SELECT
			s.id,
			s."salesExecId",
			COALESCE(se.name, 'Unknown'),
			s."fromTeamId",
			ft.name,
			s."toTeamId",
			COALESCE(tt.name, 'Unknown'),
			s."transferredById",
			COALESCE(tb.name, 'Unknown'),
			s."createdAt"
		FROM "SalesExecTeamTransfer" s
		LEFT JOIN "User" se ON se.id = s."salesExecId"
		LEFT JOIN "Team" ft ON ft.id = s."fromTeamId"
		LEFT JOIN "Team" tt ON tt.id = s."toTeamId"
		LEFT JOIN "User" tb ON tb.id = s."transferredById"
		WHERE %s
		ORDER BY s."createdAt" DESC, s.id DESC
		LIMIT $%d`, whereSQL, limitPos)

	rows, err := s.pool.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, "", false, 0, err
	}
	defer rows.Close()

	out := make([]SalesExecTeamTransferLog, 0, limit)
	for rows.Next() {
		var item SalesExecTeamTransferLog
		if err := rows.Scan(
			&item.ID,
			&item.SalesExecID,
			&item.SalesExecName,
			&item.FromTeamID,
			&item.FromTeamName,
			&item.ToTeamID,
			&item.ToTeamName,
			&item.TransferredByID,
			&item.TransferredByName,
			&item.CreatedAt,
		); err != nil {
			return nil, "", false, 0, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, 0, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	nextCursor := ""
	if hasMore && len(out) > 0 {
		last := out[len(out)-1]
		nextCursor = encodeTransferCursor(transferCursor{
			T:  last.CreatedAt.UTC().Format(time.RFC3339Nano),
			ID: last.ID,
		})
	}
	return out, nextCursor, hasMore, total, nil
}

func (s *TransferStore) ListLeadTransfers(
	ctx context.Context,
	query string,
	action string,
	cursorRaw string,
	limit int,
	createdByID string,
	teamID string,
) ([]LeadTransferLog, string, bool, int64, error) {
	limit = clampTransferLimit(limit)
	q := strings.TrimSpace(query)
	action = strings.TrimSpace(action)
	if action != "" && action != "all" {
		if _, ok := leadTransferActionLabels[action]; !ok {
			return nil, "", false, 0, fmt.Errorf("invalid action")
		}
	} else {
		action = ""
	}
	owner := strings.TrimSpace(createdByID)
	team := strings.TrimSpace(teamID)

	args := make([]any, 0, 10)
	args = append(args, leadTransferActions)
	where := []string{`h.action = ANY($1)`}

	if team != "" {
		args = append(args, team)
		where = append(where, fmt.Sprintf(`l."teamId" = $%d`, len(args)))
	} else if owner != "" {
		args = append(args, owner)
		where = append(where, fmt.Sprintf(`l."createdById" = $%d`, len(args)))
	}

	if action != "" {
		args = append(args, action)
		where = append(where, fmt.Sprintf(`h.action = $%d`, len(args)))
	}

	if q != "" {
		args = append(args, "%"+strings.ToLower(q)+"%")
		n := len(args)
		where = append(where, fmt.Sprintf(`(
			lower(COALESCE(l."leadName", '')) LIKE $%d
			OR lower(COALESCE(a.name, '')) LIKE $%d
			OR lower(COALESCE(h.detail, '')) LIKE $%d
			OR lower(h.action) LIKE $%d
		)`, n, n, n, n))
	}

	cur, err := decodeTransferCursor(cursorRaw)
	if err != nil {
		return nil, "", false, 0, err
	}
	if cur.T != "" && cur.ID != "" {
		t, err := time.Parse(time.RFC3339Nano, cur.T)
		if err != nil {
			t, err = time.Parse(time.RFC3339, cur.T)
			if err != nil {
				return nil, "", false, 0, fmt.Errorf("invalid cursor")
			}
		}
		args = append(args, t, cur.ID)
		a, b := len(args)-1, len(args)
		where = append(where, fmt.Sprintf(`(h."createdAt", h.id) < ($%d, $%d)`, a, b))
	}

	whereSQL := strings.Join(where, " AND ")

	// Filtered total without cursor.
	countArgs := []any{leadTransferActions}
	countWhere := []string{`h.action = ANY($1)`}
	if team != "" {
		countArgs = append(countArgs, team)
		countWhere = append(countWhere, fmt.Sprintf(`l."teamId" = $%d`, len(countArgs)))
	} else if owner != "" {
		countArgs = append(countArgs, owner)
		countWhere = append(countWhere, fmt.Sprintf(`l."createdById" = $%d`, len(countArgs)))
	}
	if action != "" {
		countArgs = append(countArgs, action)
		countWhere = append(countWhere, fmt.Sprintf(`h.action = $%d`, len(countArgs)))
	}
	if q != "" {
		countArgs = append(countArgs, "%"+strings.ToLower(q)+"%")
		n := len(countArgs)
		countWhere = append(countWhere, fmt.Sprintf(`(
			lower(COALESCE(l."leadName", '')) LIKE $%d
			OR lower(COALESCE(a.name, '')) LIKE $%d
			OR lower(COALESCE(h.detail, '')) LIKE $%d
			OR lower(h.action) LIKE $%d
		)`, n, n, n, n))
	}
	countSQL := `
		SELECT COUNT(*)::bigint
		FROM "LeadHandoffLog" h
		LEFT JOIN "Lead" l ON l.id = h."leadId"
		LEFT JOIN "User" a ON a.id = h."actorId"
		WHERE ` + strings.Join(countWhere, " AND ")

	var total int64
	if err := s.pool.QueryRow(ctx, countSQL, countArgs...).Scan(&total); err != nil {
		return nil, "", false, 0, err
	}

	args = append(args, limit+1)
	limitPos := len(args)
	listSQL := fmt.Sprintf(`
		SELECT
			h.id,
			h."leadId",
			COALESCE(NULLIF(BTRIM(l."leadName"), ''), 'Unknown lead'),
			h.action,
			h."actorId",
			a.name,
			h.detail,
			h."createdAt"
		FROM "LeadHandoffLog" h
		LEFT JOIN "Lead" l ON l.id = h."leadId"
		LEFT JOIN "User" a ON a.id = h."actorId"
		WHERE %s
		ORDER BY h."createdAt" DESC, h.id DESC
		LIMIT $%d`, whereSQL, limitPos)

	rows, err := s.pool.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, "", false, 0, err
	}
	defer rows.Close()

	out := make([]LeadTransferLog, 0, limit)
	for rows.Next() {
		var item LeadTransferLog
		if err := rows.Scan(
			&item.ID,
			&item.LeadID,
			&item.LeadName,
			&item.Action,
			&item.ActorID,
			&item.ActorName,
			&item.Detail,
			&item.CreatedAt,
		); err != nil {
			return nil, "", false, 0, err
		}
		if label, ok := leadTransferActionLabels[item.Action]; ok {
			item.ActionLabel = label
		} else {
			item.ActionLabel = item.Action
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, "", false, 0, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}
	nextCursor := ""
	if hasMore && len(out) > 0 {
		last := out[len(out)-1]
		nextCursor = encodeTransferCursor(transferCursor{
			T:  last.CreatedAt.UTC().Format(time.RFC3339Nano),
			ID: last.ID,
		})
	}
	return out, nextCursor, hasMore, total, nil
}
