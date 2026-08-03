package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	defaultLeadLimit = 40
	maxLeadLimit     = 100
)

// LeadListItem is shaped for the leads table UI (display-ready strings).
type LeadListItem struct {
	ID              string   `json:"id"`
	AnalystName     string   `json:"analystName"`
	AnalystEmail    string   `json:"analystEmail"`
	Source          string   `json:"source"`
	Portal          string   `json:"portal"`
	LeadLabel       string   `json:"leadLabel"`
	CreatedAt       string   `json:"createdAt"`
	CreatedAtRaw    string   `json:"createdAtRaw"`
	Tag             string   `json:"tag"`
	ContactPhone    string   `json:"contactPhone"`
	ContactEmail    string   `json:"contactEmail"`
	ContactLocation string   `json:"contactLocation"`
	ClientProfile   string   `json:"clientProfile"`
	AnalystNotes    string   `json:"analystNotes"`
	ExecutiveNotes  string   `json:"executiveNotes"`
	DuplicateCheck  string   `json:"duplicateCheck"`
	Status          string   `json:"status"`
	StatusRaw       string   `json:"statusRaw"`
	Score           string   `json:"score"`
	Stage           string   `json:"stage"`
	StageRaw        string   `json:"stageRaw"`
	Closed          string   `json:"closed"`
	NotAppropriate  bool     `json:"notAppropriate"`
	IP              string   `json:"ip"`
	DealValue       string   `json:"dealValue"`
	DealValueRaw    *float64 `json:"dealValueRaw"`
	Team            string   `json:"team"`
	SalesExecutive  string   `json:"salesExecutive"`
	Handoff         string   `json:"handoff"`
	IsNew           bool     `json:"isNew"`
}

type LeadListResponse struct {
	Items      []LeadListItem `json:"items"`
	NextCursor string         `json:"nextCursor,omitempty"`
	HasMore    bool           `json:"hasMore"`
	Total      int64          `json:"total"`
	Limit      int            `json:"limit"`
	Filter     string         `json:"filter"`
	Sort       string         `json:"sort"`
	Query      string         `json:"query,omitempty"`
	Field      string         `json:"field,omitempty"`
}

type LeadListParams struct {
	Filter   string
	Sort     string
	Query    string
	Field    string // optional column scope for search
	Cursor   string
	Limit    int
	ViewerID string

	// Dashboard deep-link facets (exact match unless noted).
	Country     string
	City        string
	TeamID      string // "none" = unassigned
	AnalystID   string // "none" = unassigned
	SalesExecID string // "none" = unassigned
	// ManagerID scopes to leads whose creator reports to this manager (ATL).
	ManagerID   string
	Source      string // "none" = blank/unassigned
	Portal      string // "none" = blank/unassigned
	MetaProfile string // "none" = blank/unassigned
	Status      string // exact qualificationStatus
	Stage       string // exact salesStage
	// Exact extracted qualification reason (from notes), matching dashboard reasons buckets.
	QualificationReason string
	AddedFrom           string // YYYY-MM-DD inclusive
	AddedTo             string // YYYY-MM-DD inclusive
}

type leadCursor struct {
	Sort string `json:"s"`
	T    string `json:"t"` // RFC3339Nano createdAt / or deal value string
	ID   string `json:"i"`
	N    string `json:"n,omitempty"` // analyst name for analyst sort
	V    string `json:"v,omitempty"` // deal value for value sort
	K    string `json:"k,omitempty"` // status/stage key
}

type LeadStore struct {
	pool *pgxpool.Pool
}

func NewLeadStore(pool *pgxpool.Pool) *LeadStore {
	return &LeadStore{pool: pool}
}

func clampLeadLimit(limit int) int {
	if limit <= 0 {
		return defaultLeadLimit
	}
	if limit > maxLeadLimit {
		return maxLeadLimit
	}
	return limit
}

func encodeLeadCursor(c leadCursor) string {
	raw, err := json.Marshal(c)
	if err != nil {
		return ""
	}
	return base64.RawURLEncoding.EncodeToString(raw)
}

func decodeLeadCursor(s string) (leadCursor, error) {
	var c leadCursor
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

func humanizeEnum(value string) string {
	if value == "" {
		return "—"
	}
	parts := strings.Split(strings.ToLower(value), "_")
	for i, p := range parts {
		if p == "" {
			continue
		}
		parts[i] = strings.ToUpper(p[:1]) + p[1:]
	}
	return strings.Join(parts, " ")
}

// qualificationDisplay matches lead_flow_ui QUALIFICATION_OPTIONS labels.
func qualificationDisplay(status string) string {
	switch status {
	case "QUALIFIED":
		return "Qualified"
	case "QUALIFIED_CHAT":
		return "Qualified - Chat"
	case "QUALIFIED_CALL":
		return "Qualified - Call"
	case "NOT_QUALIFIED":
		return "Not Qualified"
	case "IRRELEVANT":
		return "Irrelevant"
	default:
		return humanizeEnum(status)
	}
}

// salesStageDisplay matches lead_flow_ui LEAD_STAGE_OPTIONS labels.
func salesStageDisplay(stage string) string {
	switch stage {
	case "WITH_TEAM_LEAD":
		return "With team lead"
	case "WITH_EXECUTIVE":
		return "With executive"
	case "NOT_CONNECTED":
		return "Not Connected"
	case "IN_NEGOTIATION":
		return "In Negotiation"
	case "NO_RESPONSE_FROM_CLIENT":
		return "No Response from Client"
	case "CLOSED_WON":
		return "Closed"
	case "CLOSED_LOST":
		return "Lost"
	default:
		return humanizeEnum(stage)
	}
}

// Sales executive outcome stages (global, per-lead).
var allowedSalesOutcomes = map[string]struct{}{
	"IN_NEGOTIATION":          {},
	"NOT_CONNECTED":           {},
	"NO_RESPONSE_FROM_CLIENT": {},
	"CLOSED_WON":              {},
	"CLOSED_LOST":             {},
}

func isSalesOutcomeStage(stage string) bool {
	_, ok := allowedSalesOutcomes[strings.TrimSpace(stage)]
	return ok
}

func formatMoneyAmount(value *float64) string {
	if value == nil {
		return "—"
	}
	return strconv.FormatFloat(*value, 'f', -1, 64)
}

func formatLeadCreatedAt(t time.Time) string {
	return t.In(time.Local).Format("1/2/2006, 3:04 PM")
}

func formatDealValue(value *float64, currency string) string {
	if value == nil {
		return "—"
	}
	cur := strings.TrimSpace(currency)
	if cur == "" {
		cur = "USD"
	}
	return fmt.Sprintf("%s %s", cur, strconv.FormatFloat(*value, 'f', -1, 64))
}

func formatLocation(city, country *string) string {
	c := "—"
	co := "—"
	if city != nil && strings.TrimSpace(*city) != "" {
		c = strings.TrimSpace(*city)
	}
	if country != nil && strings.TrimSpace(*country) != "" {
		co = strings.TrimSpace(*country)
	}
	if c == "—" && co == "—" {
		return "—"
	}
	return c + ", " + co
}

func displayOrDash(value *string) string {
	if value == nil {
		return "—"
	}
	trimmed := strings.TrimSpace(*value)
	if trimmed == "" {
		return "—"
	}
	return trimmed
}

func truncateDisplay(value *string, max int) string {
	text := displayOrDash(value)
	if text == "—" || max <= 0 || len(text) <= max {
		return text
	}
	if max <= 1 {
		return text[:max]
	}
	// Avoid cutting mid-rune for typical ASCII notes; rune-safe truncate.
	runes := []rune(text)
	if len(runes) <= max {
		return text
	}
	return string(runes[:max-1]) + "…"
}

func formatLeadScore(score *int) string {
	if score == nil {
		return "—"
	}
	return strconv.Itoa(*score)
}

func formatHandoff(action, detail *string) string {
	if action == nil || strings.TrimSpace(*action) == "" {
		return "—"
	}
	label := humanizeEnum(strings.TrimSpace(*action))
	if detail == nil || strings.TrimSpace(*detail) == "" {
		return label
	}
	d := strings.TrimSpace(*detail)
	runes := []rune(d)
	if len(runes) > 72 {
		d = string(runes[:71]) + "…"
	}
	return label + " · " + d
}

func closedLabel(closedAt *time.Time, stage string) string {
	if closedAt != nil {
		return "Closed"
	}
	switch stage {
	case "CLOSED_WON", "CLOSED_LOST":
		return "Closed"
	default:
		return "Open"
	}
}

func normalizeLeadFilter(filter string) string {
	switch strings.ToLower(strings.TrimSpace(filter)) {
	case "", "all":
		return "all"
	case "new":
		return "new"
	case "qualified":
		return "qualified"
	case "irrelevant":
		return "irrelevant"
	case "not_appropriate", "not-appropriate", "inappropriate":
		return "not_appropriate"
	case "open":
		return "open"
	case "contacted":
		return "contacted"
	case "converted":
		return "converted"
	case "lost":
		return "lost"
	case "not_qualified", "not-qualified":
		return "not_qualified"
	case "passed", "total_pass", "total-pass", "handed_off", "handed-off":
		return "passed"
	case "passed_se_tl", "passed-se-tl", "passed_to_se_tl":
		return "passed_se_tl"
	case "not_passed", "not-passed", "unpassed", "not_passed_se_tl":
		return "not_passed"
	case "with_team_lead", "with-team-lead", "with_tl":
		return "with_team_lead"
	case "with_sales_exec", "with-sales-exec", "with_se":
		return "with_sales_exec"
	case "assigned", "assigned_internal", "assigned-internal":
		return "assigned"
	case "in_progress", "in-progress":
		return "in_progress"
	case "pipeline", "pipeline_all", "pipeline-all":
		return "pipeline"
	case "pipeline_assigned", "pipeline-assigned":
		return "pipeline_assigned"
	case "pipeline_in_progress", "pipeline-in-progress", "pipeline_progress", "pipeline-progress":
		return "pipeline_in_progress"
	case "pipeline_won", "pipeline-won":
		return "pipeline_won"
	case "pipeline_lost", "pipeline-lost":
		return "pipeline_lost"
	default:
		return "all"
	}
}

func normalizeLeadSort(sort string) string {
	switch strings.ToLower(strings.TrimSpace(sort)) {
	case "", "name", "name-asc", "a-z", "az":
		return "name"
	case "newest":
		return "newest"
	case "oldest":
		return "oldest"
	case "recent", "recently-updated", "activity", "last-updated":
		// Most recently mutated lead first (edit, assign, rename, stage…).
		return "recent"
	case "status":
		return "status"
	case "stage":
		return "stage"
	case "value":
		return "value"
	case "analyst":
		return "analyst"
	default:
		return "name"
	}
}

func appendLeadFilter(where *[]string, args *[]any, filter string) {
	switch filter {
	case "new", "not_qualified":
		*args = append(*args, "NOT_QUALIFIED")
		*where = append(*where, fmt.Sprintf(`l."qualificationStatus" = $%d`, len(*args)))
	case "qualified":
		*args = append(*args, "QUALIFIED", "QUALIFIED_CALL", "QUALIFIED_CHAT")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."qualificationStatus" IN ($%d, $%d, $%d)`, n-2, n-1, n))
	case "irrelevant":
		*args = append(*args, "IRRELEVANT")
		*where = append(*where, fmt.Sprintf(`l."qualificationStatus" = $%d`, len(*args)))
	case "not_appropriate":
		*where = append(*where, `l."notAppropriate" = TRUE`)
	case "open":
		*args = append(*args, "CLOSED_WON", "CLOSED_LOST")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."closedAt" IS NULL AND l."salesStage" NOT IN ($%d, $%d)`, n-1, n))
	case "contacted":
		*args = append(*args, "WITH_EXECUTIVE", "WITH_TEAM_LEAD", "IN_NEGOTIATION", "NOT_CONNECTED")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."salesStage" IN ($%d, $%d, $%d, $%d)`, n-3, n-2, n-1, n))
	case "converted":
		*args = append(*args, "CLOSED_WON")
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	case "lost":
		*args = append(*args, "CLOSED_LOST")
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	case "passed":
		// Matches LeadSummary.TotalPassed: distinct leads handed to a sales executive.
		*args = append(*args, "DIRECT_ASSIGNED_TO_EXECUTIVE_BY_ATL", "ASSIGNED_TO_EXECUTIVE")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM "LeadHandoffLog" h
			WHERE h."leadId" = l.id
			  AND h.action IN ($%d, $%d)
		)`, n-1, n))
	case "passed_se_tl":
		// Qualified leads currently with team lead or executive (matches pipeline assigned).
		appendQualifiedStatuses(where, args)
		*args = append(*args, "WITH_TEAM_LEAD", "WITH_EXECUTIVE")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."salesStage" IN ($%d, $%d)`, n-1, n))
	case "not_passed":
		// Not yet handed to a team lead or sales executive.
		*args = append(*args, "PRE_SALES")
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	case "with_team_lead":
		*args = append(*args, "WITH_TEAM_LEAD")
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	case "with_sales_exec":
		*args = append(*args, "WITH_EXECUTIVE")
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	case "assigned":
		// Same inventory as Passed to SE/TLs (with team lead / with executive).
		*args = append(*args, "WITH_TEAM_LEAD", "WITH_EXECUTIVE")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."salesStage" IN ($%d, $%d)`, n-1, n))
	case "in_progress":
		*args = append(*args, "NOT_CONNECTED", "IN_NEGOTIATION", "NO_RESPONSE_FROM_CLIENT")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."salesStage" IN ($%d, $%d, $%d)`, n-2, n-1, n))
	case "pipeline":
		appendQualifiedStatuses(where, args)
	case "pipeline_assigned":
		// Qualified leads currently with TL or SE — matches dashboard “Passed to SE/TLs”.
		appendQualifiedStatuses(where, args)
		*args = append(*args, "WITH_TEAM_LEAD", "WITH_EXECUTIVE")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."salesStage" IN ($%d, $%d)`, n-1, n))
	case "pipeline_in_progress":
		appendQualifiedStatuses(where, args)
		*args = append(*args, "NOT_CONNECTED", "IN_NEGOTIATION", "NO_RESPONSE_FROM_CLIENT")
		n := len(*args)
		*where = append(*where, fmt.Sprintf(`l."salesStage" IN ($%d, $%d, $%d)`, n-2, n-1, n))
	case "pipeline_won":
		appendQualifiedStatuses(where, args)
		*args = append(*args, "CLOSED_WON")
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	case "pipeline_lost":
		appendQualifiedStatuses(where, args)
		*args = append(*args, "CLOSED_LOST")
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	}
}

func appendQualifiedStatuses(where *[]string, args *[]any) {
	*args = append(*args, "QUALIFIED", "QUALIFIED_CALL", "QUALIFIED_CHAT")
	n := len(*args)
	*where = append(*where, fmt.Sprintf(`l."qualificationStatus" IN ($%d, $%d, $%d)`, n-2, n-1, n))
}

// escapeILIKE escapes \, %, and _ so user input is matched literally.
func escapeILIKE(s string) string {
	s = strings.ReplaceAll(s, `\`, `\\`)
	s = strings.ReplaceAll(s, `%`, `\%`)
	s = strings.ReplaceAll(s, `_`, `\_`)
	return s
}

func digitsOnly(s string) string {
	return strings.Map(func(r rune) rune {
		if r >= '0' && r <= '9' {
			return r
		}
		return -1
	}, s)
}

func appendBlankOrEqual(where *[]string, args *[]any, expr, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if strings.EqualFold(value, "none") || strings.EqualFold(value, "unassigned") {
		*where = append(*where, fmt.Sprintf(`(%s IS NULL OR BTRIM(%s) = '')`, expr, expr))
		return
	}
	*args = append(*args, value)
	*where = append(*where, fmt.Sprintf(`BTRIM(%s) = $%d`, expr, len(*args)))
}

func appendNullableID(where *[]string, args *[]any, column, value string) {
	value = strings.TrimSpace(value)
	if value == "" {
		return
	}
	if strings.EqualFold(value, "none") || strings.EqualFold(value, "unassigned") {
		*where = append(*where, fmt.Sprintf(`%s IS NULL`, column))
		return
	}
	*args = append(*args, value)
	*where = append(*where, fmt.Sprintf(`%s = $%d`, column, len(*args)))
}

func appendLeadFacets(where *[]string, args *[]any, params LeadListParams) {
	geo := parseGeoFilter(params.Country, params.City)
	if geo.Country != "" {
		if isBlankGeoLabel(geo.Country) {
			*where = append(*where, blankGeoSQL("l.", "country"))
		} else {
			*args = append(*args, geo.Country)
			*where = append(*where, fmt.Sprintf(`BTRIM(l.country) = $%d`, len(*args)))
		}
	}
	if geo.City != "" {
		if isBlankGeoLabel(geo.City) {
			*where = append(*where, blankGeoSQL("l.", "city"))
		} else {
			*args = append(*args, geo.City)
			*where = append(*where, fmt.Sprintf(`BTRIM(l.city) = $%d`, len(*args)))
		}
	}

	appendNullableID(where, args, `l."teamId"`, params.TeamID)
	appendNullableID(where, args, `l."createdById"`, params.AnalystID)
	appendNullableID(where, args, `l."assignedSalesExecId"`, params.SalesExecID)
	if managerID := strings.TrimSpace(params.ManagerID); managerID != "" {
		*args = append(*args, managerID)
		*where = append(*where, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM "User" creator
			WHERE creator.id = l."createdById"
			  AND creator."managerId" = $%d
		)`, len(*args)))
	}
	appendBlankOrEqual(where, args, `l.source`, params.Source)
	appendBlankOrEqual(where, args, `l."portalWebsite"`, params.Portal)
	appendBlankOrEqual(where, args, `l."sourceMetaProfileName"`, params.MetaProfile)

	if status := strings.TrimSpace(params.Status); status != "" {
		*args = append(*args, strings.ToUpper(status))
		*where = append(*where, fmt.Sprintf(`l."qualificationStatus" = $%d`, len(*args)))
	}
	if stage := strings.TrimSpace(params.Stage); stage != "" {
		*args = append(*args, strings.ToUpper(stage))
		*where = append(*where, fmt.Sprintf(`l."salesStage" = $%d`, len(*args)))
	}

	from := strings.TrimSpace(params.AddedFrom)
	to := strings.TrimSpace(params.AddedTo)
	if from != "" && to != "" && from > to {
		from, to = to, from
	}
	if from != "" {
		if _, err := time.Parse("2006-01-02", from); err == nil {
			*args = append(*args, from)
			*where = append(*where, fmt.Sprintf(`l."createdAt" >= $%d::date`, len(*args)))
		}
	}
	if to != "" {
		if _, err := time.Parse("2006-01-02", to); err == nil {
			*args = append(*args, to)
			// Inclusive end-of-day: createdAt < (to + 1 day)
			*where = append(*where, fmt.Sprintf(`l."createdAt" < ($%d::date + INTERVAL '1 day')`, len(*args)))
		}
	}

	appendQualificationReasonFacet(where, args, params.QualificationReason)
}

func appendQualificationReasonFacet(where *[]string, args *[]any, reason string) {
	reason = strings.TrimSpace(reason)
	if reason == "" {
		return
	}
	extracted := `COALESCE(` + rawQualificationReasonSQL + `, 'No reason recorded')`
	if strings.EqualFold(reason, "No reason recorded") ||
		strings.EqualFold(reason, "none") ||
		strings.EqualFold(reason, "unassigned") {
		*where = append(*where, extracted+` = 'No reason recorded'`)
		return
	}
	*args = append(*args, reason)
	*where = append(*where, fmt.Sprintf(`%s = $%d`, extracted, len(*args)))
}

// buildLeadListWhere builds filter/search/facet clauses with placeholders starting after reserved.
// reserved is the count of args already allocated (e.g. 1 when $1 is viewer id).
func buildLeadListWhere(params LeadListParams, reserved int) ([]string, []any) {
	args := make([]any, 0, 20+reserved)
	for i := 0; i < reserved; i++ {
		args = append(args, nil)
	}
	where := make([]string, 0, 12)

	// Exact qualification status supersedes coarse filter presets.
	if strings.TrimSpace(params.Status) == "" {
		appendLeadFilter(&where, &args, normalizeLeadFilter(params.Filter))
	}
	appendLeadFacets(&where, &args, params)
	appendLeadSearch(&where, &args, params.Query, params.Field)

	if reserved > 0 {
		return where, args[reserved:]
	}
	return where, args
}

func parseCursorTime(value string) (time.Time, error) {
	t, err := time.Parse(time.RFC3339Nano, value)
	if err == nil {
		return t, nil
	}
	return time.Parse(time.RFC3339, value)
}

func (s *LeadStore) List(ctx context.Context, params LeadListParams) (LeadListResponse, error) {
	filter := normalizeLeadFilter(params.Filter)
	sort := normalizeLeadSort(params.Sort)
	limit := clampLeadLimit(params.Limit)
	q := strings.TrimSpace(params.Query)
	field := canonicalSearchField(params.Field)
	viewerID := strings.TrimSpace(params.ViewerID)
	params.Filter = filter
	params.Sort = sort
	params.Query = q
	params.Field = field

	cur, err := decodeLeadCursor(params.Cursor)
	if err != nil {
		return LeadListResponse{}, err
	}
	if cur.ID != "" && cur.Sort != "" && cur.Sort != sort {
		// Ignore stale cursors from a previous sort mode.
		cur = leadCursor{}
	}

	where, filterArgs := buildLeadListWhere(params, 0)

	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	hasFacets := params.Country != "" || params.City != "" ||
		params.TeamID != "" || params.AnalystID != "" || params.SalesExecID != "" ||
		params.Source != "" || params.Portal != "" || params.MetaProfile != "" ||
		params.Status != "" || params.Stage != "" ||
		params.AddedFrom != "" || params.AddedTo != ""

	// Run the total count concurrently with the page query — it shares no
	// state with the list below (which appends cursor/viewer args to a copy).
	var total int64
	countDone := make(chan error, 1)
	if filter == "all" && q == "" && !hasFacets && whereSQL == "" {
		go func() {
			err := s.pool.QueryRow(ctx, `
				SELECT COALESCE(reltuples, 0)::bigint
				FROM pg_class
				WHERE oid = '"Lead"'::regclass`).Scan(&total)
			if err == nil && total < 0 {
				total = 0
			}
			countDone <- err
		}()
	} else {
		countSQL := `SELECT COUNT(*)::bigint FROM "Lead" l ` + whereSQL
		countArgs := append([]any{}, filterArgs...)
		go func() {
			countDone <- s.pool.QueryRow(ctx, countSQL, countArgs...).Scan(&total)
		}()
	}

	viewerJoin := ""
	viewerSelect := "FALSE"
	args := append([]any{}, filterArgs...)
	if viewerID != "" {
		listWhere, listArgs := buildLeadListWhere(params, 1)
		args = append([]any{viewerID}, listArgs...)
		whereSQL = ""
		if len(listWhere) > 0 {
			whereSQL = "WHERE " + strings.Join(listWhere, " AND ")
		}
		viewerJoin = `LEFT JOIN "LeadView" lv ON lv."leadId" = l.id AND lv."userId" = $1`
		viewerSelect = `(lv."viewedAt" IS NULL AND l."createdAt" > NOW() - INTERVAL '30 days')`
	}

	cursorSQL := ""
	orderSQL := ""
	switch sort {
	case "name":
		orderSQL = `ORDER BY LOWER(l."leadName") ASC, l.id ASC`
		if cur.ID != "" {
			args = append(args, strings.ToLower(cur.N), cur.ID)
			a, b := len(args)-1, len(args)
			cursorSQL = fmt.Sprintf(` AND (
				LOWER(l."leadName") > $%d
				OR (LOWER(l."leadName") = $%d AND l.id > $%d)
			)`, a, a, b)
		}
	case "oldest":
		orderSQL = `ORDER BY l."createdAt" ASC, l.id ASC`
		if cur.ID != "" && cur.T != "" {
			if t, err := parseCursorTime(cur.T); err == nil {
				args = append(args, t, cur.ID)
				a, b := len(args)-1, len(args)
				cursorSQL = fmt.Sprintf(` AND (l."createdAt", l.id) > ($%d, $%d)`, a, b)
			}
		}
	case "recent":
		// updatedAt is bumped on every lead mutation (edit, assign, rename,
		// qualify, close…). Uses Lead_updatedAt_id_desc_idx.
		orderSQL = `ORDER BY l."updatedAt" DESC, l.id DESC`
		if cur.ID != "" && cur.T != "" {
			if t, err := parseCursorTime(cur.T); err == nil {
				args = append(args, t, cur.ID)
				a, b := len(args)-1, len(args)
				cursorSQL = fmt.Sprintf(` AND (l."updatedAt", l.id) < ($%d, $%d)`, a, b)
			}
		}
	case "status":
		orderSQL = `ORDER BY l."qualificationStatus" ASC, l."createdAt" DESC, l.id DESC`
		if cur.ID != "" && cur.K != "" && cur.T != "" {
			if t, err := parseCursorTime(cur.T); err == nil {
				args = append(args, cur.K, t, cur.ID)
				a, b, c := len(args)-2, len(args)-1, len(args)
				cursorSQL = fmt.Sprintf(` AND (
					l."qualificationStatus" > $%d
					OR (l."qualificationStatus" = $%d AND (l."createdAt", l.id) < ($%d, $%d))
				)`, a, a, b, c)
			}
		}
	case "stage":
		orderSQL = `ORDER BY l."salesStage" ASC, l."createdAt" DESC, l.id DESC`
		if cur.ID != "" && cur.K != "" && cur.T != "" {
			if t, err := parseCursorTime(cur.T); err == nil {
				args = append(args, cur.K, t, cur.ID)
				a, b, c := len(args)-2, len(args)-1, len(args)
				cursorSQL = fmt.Sprintf(` AND (
					l."salesStage" > $%d
					OR (l."salesStage" = $%d AND (l."createdAt", l.id) < ($%d, $%d))
				)`, a, a, b, c)
			}
		}
	case "value":
		orderSQL = `ORDER BY l."estimatedDealValue" DESC NULLS LAST, l."createdAt" DESC, l.id DESC`
		if cur.ID != "" && cur.T != "" {
			if t, err := parseCursorTime(cur.T); err == nil {
				var deal any
				if cur.V != "" {
					if f, perr := strconv.ParseFloat(cur.V, 64); perr == nil {
						deal = f
					}
				}
				args = append(args, deal, t, cur.ID)
				a, b, c := len(args)-2, len(args)-1, len(args)
				cursorSQL = fmt.Sprintf(` AND (
					($%d::numeric IS NOT NULL AND (
						l."estimatedDealValue" IS NULL
						OR l."estimatedDealValue" < $%d
						OR (l."estimatedDealValue" = $%d AND (l."createdAt", l.id) < ($%d, $%d))
					))
					OR ($%d::numeric IS NULL AND l."estimatedDealValue" IS NULL AND (l."createdAt", l.id) < ($%d, $%d))
				)`, a, a, a, b, c, a, b, c)
			}
		}
	case "analyst":
		orderSQL = `ORDER BY COALESCE(cb.name, '') ASC, l."createdAt" DESC, l.id DESC`
		if cur.ID != "" && cur.T != "" {
			if t, err := parseCursorTime(cur.T); err == nil {
				args = append(args, cur.N, t, cur.ID)
				a, b, c := len(args)-2, len(args)-1, len(args)
				cursorSQL = fmt.Sprintf(` AND (
					COALESCE(cb.name, '') > $%d
					OR (COALESCE(cb.name, '') = $%d AND (l."createdAt", l.id) < ($%d, $%d))
				)`, a, a, b, c)
			}
		}
	default: // newest — uses Lead_createdAt_id_desc_idx
		orderSQL = `ORDER BY l."createdAt" DESC, l.id DESC`
		if cur.ID != "" && cur.T != "" {
			if t, err := parseCursorTime(cur.T); err == nil {
				args = append(args, t, cur.ID)
				a, b := len(args)-1, len(args)
				cursorSQL = fmt.Sprintf(` AND (l."createdAt", l.id) < ($%d, $%d)`, a, b)
			}
		}
	}

	if cursorSQL != "" {
		if whereSQL == "" {
			whereSQL = "WHERE TRUE" + cursorSQL
		} else {
			whereSQL = whereSQL + cursorSQL
		}
	}

	args = append(args, limit+1)
	limitPos := len(args)

	listSQL := fmt.Sprintf(`
		SELECT
			l.id,
			l."leadName",
			l.phone,
			l."leadEmail",
			l.country,
			l.city,
			l.source,
			l."portalWebsite",
			l."clientProfile",
			l.notes,
			l."lostNotes",
			l."qualificationStatus",
			l."leadScore",
			l."salesStage",
			l."estimatedDealValue",
			l."closedRevenue",
			l."initialPayment",
			l."dealCurrency",
			l."closedAt",
			l."createdAt",
			l."updatedAt",
			l."notAppropriate",
			COALESCE(cb.name, '—'),
			COALESCE(cb.email, '—'),
			COALESCE(t.name, '—'),
			COALESCE(se.name, '—'),
			hh.action,
			hh.detail,
			%s AS is_new
		FROM "Lead" l
		LEFT JOIN "User" cb ON cb.id = l."createdById"
		LEFT JOIN "Team" t ON t.id = l."teamId"
		LEFT JOIN "User" se ON se.id = l."assignedSalesExecId"
		%s
		LEFT JOIN LATERAL (
			SELECT h.action, h.detail
			FROM "LeadHandoffLog" h
			WHERE h."leadId" = l.id
			  AND h.action <> 'LEAD_CREATED'
			ORDER BY h."createdAt" DESC
			LIMIT 1
		) hh ON TRUE
		%s
		%s
		LIMIT $%d`, viewerSelect, viewerJoin, whereSQL, orderSQL, limitPos)

	rows, err := s.pool.Query(ctx, listSQL, args...)
	if err != nil {
		return LeadListResponse{}, err
	}
	defer rows.Close()

	items := make([]LeadListItem, 0, limit)
	type rawRow struct {
		id, leadName, source, status, stage, currency string
		phone, email, country, city                   *string
		portal, clientProfile                         *string
		notes, lostNotes                              *string
		score                                         *int
		deal, closedRevenue, initialPayment           *float64
		closedAt                                      *time.Time
		createdAt                                     time.Time
		updatedAt                                     time.Time
		notAppropriate                                bool
		analystName, analystEmail, team, exec         string
		handoffAction, handoffDetail                  *string
		isNew                                         bool
	}
	raws := make([]rawRow, 0, limit)

	for rows.Next() {
		var r rawRow
		if err := rows.Scan(
			&r.id, &r.leadName, &r.phone, &r.email, &r.country, &r.city, &r.source,
			&r.portal, &r.clientProfile, &r.notes, &r.lostNotes,
			&r.status, &r.score, &r.stage, &r.deal, &r.closedRevenue, &r.initialPayment, &r.currency, &r.closedAt, &r.createdAt, &r.updatedAt,
			&r.notAppropriate,
			&r.analystName, &r.analystEmail, &r.team, &r.exec,
			&r.handoffAction, &r.handoffDetail, &r.isNew,
		); err != nil {
			return LeadListResponse{}, err
		}
		raws = append(raws, r)
	}
	if err := rows.Err(); err != nil {
		return LeadListResponse{}, err
	}

	hasMore := len(raws) > limit
	if hasMore {
		raws = raws[:limit]
	}

	for _, r := range raws {
		emailDisplay := r.analystEmail
		if emailDisplay == "" {
			emailDisplay = "—"
		}
		tag := "—"
		if r.notAppropriate {
			tag = "Not appropriate"
		} else if r.isNew {
			tag = "New"
		}
		items = append(items, LeadListItem{
			ID:              r.id,
			AnalystName:     r.analystName,
			AnalystEmail:    emailDisplay,
			Source:          r.source,
			Portal:          displayOrDash(r.portal),
			LeadLabel:       r.leadName,
			CreatedAt:       formatLeadCreatedAt(r.createdAt),
			CreatedAtRaw:    r.createdAt.UTC().Format(time.RFC3339Nano),
			Tag:             tag,
			ContactPhone:    displayOrDash(r.phone),
			ContactEmail:    displayOrDash(r.email),
			ContactLocation: formatLocation(r.city, r.country),
			ClientProfile:   displayOrDash(r.clientProfile),
			AnalystNotes:    truncateDisplay(r.notes, 90),
			ExecutiveNotes:  truncateDisplay(r.lostNotes, 90),
			DuplicateCheck:  "—",
			Status:          qualificationDisplay(r.status),
			StatusRaw:       r.status,
			Score:           formatLeadScore(r.score),
			Stage:           salesStageDisplay(r.stage),
			StageRaw:        r.stage,
			Closed:          closedLabel(r.closedAt, r.stage),
			NotAppropriate:  r.notAppropriate,
			IP:              formatMoneyAmount(r.initialPayment),
			DealValue: formatDealValue(func() *float64 {
				if r.closedRevenue != nil {
					return r.closedRevenue
				}
				return r.deal
			}(), r.currency),
			DealValueRaw: func() *float64 {
				if r.closedRevenue != nil {
					return r.closedRevenue
				}
				return r.deal
			}(),
			Team:           r.team,
			SalesExecutive: r.exec,
			Handoff:        formatHandoff(r.handoffAction, r.handoffDetail),
			IsNew:          r.isNew,
		})
	}

	nextCursor := ""
	if hasMore && len(raws) > 0 {
		last := raws[len(raws)-1]
		c := leadCursor{
			Sort: sort,
			T:    last.createdAt.UTC().Format(time.RFC3339Nano),
			ID:   last.id,
		}
		switch sort {
		case "name":
			c.N = last.leadName
		case "recent":
			c.T = last.updatedAt.UTC().Format(time.RFC3339Nano)
		case "status":
			c.K = last.status
		case "stage":
			c.K = last.stage
		case "value":
			if last.deal != nil {
				c.V = strconv.FormatFloat(*last.deal, 'f', -1, 64)
			}
		case "analyst":
			c.N = last.analystName
		}
		nextCursor = encodeLeadCursor(c)
	}

	// Join the concurrent count before touching total (happens-before via chan).
	if err := <-countDone; err != nil {
		return LeadListResponse{}, err
	}

	return LeadListResponse{
		Items:      items,
		NextCursor: nextCursor,
		HasMore:    hasMore,
		Total:      total,
		Limit:      limit,
		Filter:     filter,
		Sort:       sort,
		Query:      q,
		Field:      field,
	}, nil
}

type LeadSummary struct {
	ActiveUsers       int64 `json:"activeUsers"`
	LeadsTotal        int64 `json:"leadsTotal"`
	IrrelevantLeads   int64 `json:"irrelevantLeads"`
	QualifiedLeads    int64 `json:"qualifiedLeads"`
	NotQualifiedLeads int64 `json:"notQualifiedLeads"`
	TotalPassed       int64 `json:"totalPassed"`
	// Current sales-stage inventory (matches leads page stage filter).
	WithTeamLeads  int64   `json:"withTeamLeads"`
	WithSalesExecs int64   `json:"withSalesExecs"`
	TotalLost      int64   `json:"totalLost"`
	ClosedRevenue  float64 `json:"closedRevenue"`
	TotalWon       int64   `json:"totalWon"`
	NoResponse     int64   `json:"noResponse"`
	// Kept for older clients / overview deltas.
	OpenLeads        int64           `json:"openLeads"`
	DealValueSum     float64         `json:"dealValueSum"`
	LeadsLast7Days   int64           `json:"leadsLast7Days"`
	QualificationMix []NamedCount    `json:"qualificationMix"`
	TeamMix          []TeamLeadCount `json:"teamMix"`
	// Heavy mixes (reasons, attribution, analysts, sales execs) are served via
	// /api/leads/summary/buckets so the KPI payload stays fast.
	SourceMix []AttributionStats `json:"sourceMix"`
}

// PipelineSummary is the analyst pipeline board counts (mutually exclusive stage buckets).
type PipelineSummary struct {
	AssignedInternal int64 `json:"assignedInternal"`
	InProgress       int64 `json:"inProgress"`
	ClosedWon        int64 `json:"closedWon"`
	ClosedLost       int64 `json:"closedLost"`
	Total            int64 `json:"total"`
}

func (s *LeadStore) PipelineSummary(ctx context.Context, params LeadListParams) (PipelineSummary, error) {
	// Pipeline board is qualified leads only; cards ignore other stage/status facets.
	params.Filter = "pipeline"
	params.Stage = ""
	params.Status = ""
	where, args := leadScopeWhere(params, true)
	var out PipelineSummary
	err := s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*) FILTER (
				WHERE l."salesStage" IN ('WITH_TEAM_LEAD', 'WITH_EXECUTIVE')
			)::bigint,
			COUNT(*) FILTER (
				WHERE l."salesStage" IN ('NOT_CONNECTED', 'IN_NEGOTIATION', 'NO_RESPONSE_FROM_CLIENT')
			)::bigint,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::bigint,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_LOST')::bigint,
			COUNT(*)::bigint
		FROM "Lead" l `+where, args...).Scan(
		&out.AssignedInternal,
		&out.InProgress,
		&out.ClosedWon,
		&out.ClosedLost,
		&out.Total,
	)
	return out, err
}

const (
	defaultSummaryBucketLimit = 50
	maxSummaryBucketLimit     = 200
)

// SummaryBucketPage is one page of a high-cardinality dashboard mix.
type SummaryBucketPage struct {
	Dimension   string `json:"dimension"`
	Offset      int    `json:"offset"`
	Limit       int    `json:"limit"`
	HasMore     bool   `json:"hasMore"`
	BucketCount int    `json:"bucketCount"`
	LeadTotal   int    `json:"leadTotal"`
	WonTotal    int    `json:"wonTotal,omitempty"`
	Items       any    `json:"items"`
}

func clampSummaryBucketLimit(limit int) int {
	if limit <= 0 {
		return defaultSummaryBucketLimit
	}
	if limit > maxSummaryBucketLimit {
		return maxSummaryBucketLimit
	}
	return limit
}

func clampSummaryBucketOffset(offset int) int {
	if offset < 0 {
		return 0
	}
	const maxOffset = 5000
	if offset > maxOffset {
		return maxOffset
	}
	return offset
}

// AttributionStats is volume + win conversion for a channel dimension.
type AttributionStats struct {
	Name       string  `json:"name"`
	Total      int     `json:"total"`
	Won        int     `json:"won"`
	Conversion float64 `json:"conversion"` // won / total * 100
}

type TeamLeadCount struct {
	ID    string `json:"id"`
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type AnalystLeadStats struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Email        string `json:"email"`
	Total        int    `json:"total"`
	Qualified    int    `json:"qualified"`
	NotQualified int    `json:"notQualified"`
	Irrelevant   int    `json:"irrelevant"`
}

type SalesExecOutcome struct {
	ID       string `json:"id"`
	Name     string `json:"name"`
	Assigned int    `json:"assigned"`
	WithRep  int    `json:"withRep"`
	Won      int    `json:"won"`
	Lost     int    `json:"lost"`
}

type StatusReasonCount struct {
	Status string `json:"status"`
	Reason string `json:"reason"`
	Count  int    `json:"count"`
}

// leadScopeWhere builds WHERE clauses for list/summary using the same facet rules.
func leadScopeWhere(params LeadListParams, applyPreset bool) (whereSQL string, args []any) {
	where := make([]string, 0, 12)
	args = make([]any, 0, 12)
	if applyPreset {
		appendLeadFilter(&where, &args, normalizeLeadFilter(params.Filter))
	}
	appendLeadFacets(&where, &args, params)
	if len(where) == 0 {
		return "", nil
	}
	return "WHERE " + strings.Join(where, " AND "), args
}

func (s *LeadStore) Summary(ctx context.Context, params LeadListParams, hideActiveUsers bool) (LeadSummary, error) {
	var out LeadSummary

	// Active users: workspace-wide, or team-scoped when TeamID facet is forced.
	if !hideActiveUsers {
		teamID := strings.TrimSpace(params.TeamID)
		if teamID != "" && teamID != "none" {
			if err := s.pool.QueryRow(ctx, `
				SELECT COUNT(*)::bigint
				FROM "User"
				WHERE "teamId" = $1
				  AND "activeSessionHash" IS NOT NULL
				  AND "activeSessionHash" <> ''`, teamID).Scan(&out.ActiveUsers); err != nil {
				return out, err
			}
		} else if err := s.pool.QueryRow(ctx, `
			SELECT COUNT(*)::bigint
			FROM "User"
			WHERE "activeSessionHash" IS NOT NULL AND "activeSessionHash" <> ''`).Scan(&out.ActiveUsers); err != nil {
			return out, err
		}
	}

	// Total passed (legacy SE handoff) — same lead facets as other KPIs.
	passArgs := []any{"DIRECT_ASSIGNED_TO_EXECUTIVE_BY_ATL", "ASSIGNED_TO_EXECUTIVE"}
	passWhere := make([]string, 0, 8)
	appendLeadFilter(&passWhere, &passArgs, normalizeLeadFilter(params.Filter))
	appendLeadFacets(&passWhere, &passArgs, params)
	passSQL := `
		SELECT COUNT(DISTINCT h."leadId")::bigint
		FROM "LeadHandoffLog" h
		INNER JOIN "Lead" l ON l.id = h."leadId"
		WHERE h.action IN ($1, $2)`
	if len(passWhere) > 0 {
		passSQL += " AND " + strings.Join(passWhere, " AND ")
	}
	if err := s.pool.QueryRow(ctx, passSQL, passArgs...).Scan(&out.TotalPassed); err != nil {
		return out, err
	}

	leadWhere, leadArgs := leadScopeWhere(params, true)
	err := s.pool.QueryRow(ctx, `
		SELECT
			COUNT(*)::bigint,
			COUNT(*) FILTER (WHERE l."qualificationStatus" = 'IRRELEVANT')::bigint,
			COUNT(*) FILTER (
				WHERE l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CALL', 'QUALIFIED_CHAT')
			)::bigint,
			COUNT(*) FILTER (WHERE l."qualificationStatus" = 'NOT_QUALIFIED')::bigint,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_LOST')::bigint,
			COALESCE(SUM(l."closedRevenue") FILTER (
				WHERE l."closedRevenue" IS NOT NULL
			), 0)::float8,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::bigint,
			COUNT(*) FILTER (WHERE l."salesStage" = 'NO_RESPONSE_FROM_CLIENT')::bigint,
			COUNT(*) FILTER (
				WHERE l."closedAt" IS NULL
				  AND l."salesStage" NOT IN ('CLOSED_WON', 'CLOSED_LOST')
			)::bigint,
			COALESCE(SUM(l."estimatedDealValue") FILTER (
				WHERE l."estimatedDealValue" IS NOT NULL
				  AND l."closedAt" IS NULL
				  AND l."salesStage" NOT IN ('CLOSED_WON', 'CLOSED_LOST')
			), 0)::float8,
			COUNT(*) FILTER (WHERE l."createdAt" > NOW() - INTERVAL '7 days')::bigint,
			COUNT(*) FILTER (
				WHERE l."salesStage" = 'WITH_TEAM_LEAD'
				  AND l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CALL', 'QUALIFIED_CHAT')
			)::bigint,
			COUNT(*) FILTER (
				WHERE l."salesStage" = 'WITH_EXECUTIVE'
				  AND l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CALL', 'QUALIFIED_CHAT')
			)::bigint
		FROM "Lead" l `+leadWhere, leadArgs...).Scan(
		&out.LeadsTotal,
		&out.IrrelevantLeads,
		&out.QualifiedLeads,
		&out.NotQualifiedLeads,
		&out.TotalLost,
		&out.ClosedRevenue,
		&out.TotalWon,
		&out.NoResponse,
		&out.OpenLeads,
		&out.DealValueSum,
		&out.LeadsLast7Days,
		&out.WithTeamLeads,
		&out.WithSalesExecs,
	)
	if err != nil {
		return out, err
	}

	mix, err := namedCountsArgs(ctx, s.pool, `
		SELECT l."qualificationStatus", COUNT(*)::int
		FROM "Lead" l
		`+leadWhere+`
		GROUP BY 1
		ORDER BY 2 DESC`, leadArgs...)
	if err != nil {
		return out, err
	}
	out.QualificationMix = mix

	teamMix, err := s.teamLeadCounts(ctx, params)
	if err != nil {
		return out, err
	}
	out.TeamMix = teamMix

	sourceMix, err := s.attributionMix(ctx, params, `l.source`)
	if err != nil {
		return out, err
	}
	out.SourceMix = sourceMix

	return out, nil
}

func (s *LeadStore) attributionMix(ctx context.Context, params LeadListParams, columnSQL string) ([]AttributionStats, error) {
	where, args := leadScopeWhere(params, true)
	query := `
		SELECT
			COALESCE(NULLIF(BTRIM(` + columnSQL + `), ''), 'Unassigned') AS name,
			COUNT(*)::int AS total,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::int AS won
		FROM "Lead" l
		` + where + `
		GROUP BY 1
		ORDER BY 2 DESC, 1 ASC`

	rows, err := s.pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AttributionStats, 0, 64)
	for rows.Next() {
		var item AttributionStats
		if err := rows.Scan(&item.Name, &item.Total, &item.Won); err != nil {
			return nil, err
		}
		if item.Total > 0 {
			item.Conversion = float64(item.Won) * 100 / float64(item.Total)
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *LeadStore) GeographyMix(ctx context.Context, filter GeoFilter) ([]NamedCount, error) {
	where, args := filter.whereSQL("")
	return namedCountsArgs(ctx, s.pool, `
		SELECT `+normalizedCountrySQL("country")+`, COUNT(*)::int
		FROM "Lead"
		`+where+`
		GROUP BY 1
		ORDER BY 2 DESC, 1 ASC`, args...)
}

type TimeBucketCount struct {
	Key   string `json:"key"`   // YYYY-MM-DD or YYYY-MM
	Label string `json:"label"` // display label
	Count int    `json:"count"`
}

type AddedSeriesResponse struct {
	Granularity string            `json:"granularity"` // day | month
	Items       []TimeBucketCount `json:"items"`
	Total       int               `json:"total"`
	Peak        int               `json:"peak"`
	Average     float64           `json:"average"`
}

func (s *LeadStore) AddedSeries(ctx context.Context, granularity string, filter GeoFilter) (AddedSeriesResponse, error) {
	granularity = strings.ToLower(strings.TrimSpace(granularity))
	if granularity != "month" {
		granularity = "day"
	}

	geoAnd, geoArgs := filter.andSQL("l.", 0)

	var (
		query string
		out   AddedSeriesResponse
	)
	out.Granularity = granularity

	if granularity == "month" {
		// Last 12 calendar months inclusive of current month.
		query = `
			WITH bounds AS (
				SELECT
					date_trunc('month', CURRENT_DATE)::date AS end_month,
					(date_trunc('month', CURRENT_DATE) - INTERVAL '11 months')::date AS start_month
			),
			months AS (
				SELECT generate_series(
					(SELECT start_month FROM bounds),
					(SELECT end_month FROM bounds),
					'1 month'::interval
				)::date AS bucket
			),
			counts AS (
				SELECT date_trunc('month', l."createdAt")::date AS bucket, COUNT(*)::int AS cnt
				FROM "Lead" l
				CROSS JOIN bounds b
				WHERE l."createdAt" >= b.start_month
				  AND l."createdAt" < (b.end_month + INTERVAL '1 month')` + geoAnd + `
				GROUP BY 1
			)
			SELECT
				to_char(m.bucket, 'YYYY-MM') AS key,
				to_char(m.bucket, 'Mon YYYY') AS label,
				COALESCE(c.cnt, 0)::int AS count
			FROM months m
			LEFT JOIN counts c ON c.bucket = m.bucket
			ORDER BY m.bucket ASC`
	} else {
		// Last 30 calendar days inclusive of today.
		query = `
			WITH bounds AS (
				SELECT
					CURRENT_DATE AS end_day,
					(CURRENT_DATE - INTERVAL '29 days')::date AS start_day
			),
			days AS (
				SELECT generate_series(
					(SELECT start_day FROM bounds),
					(SELECT end_day FROM bounds),
					'1 day'::interval
				)::date AS bucket
			),
			counts AS (
				SELECT (l."createdAt")::date AS bucket, COUNT(*)::int AS cnt
				FROM "Lead" l
				CROSS JOIN bounds b
				WHERE l."createdAt" >= b.start_day
				  AND l."createdAt" < (b.end_day + INTERVAL '1 day')` + geoAnd + `
				GROUP BY 1
			)
			SELECT
				to_char(d.bucket, 'YYYY-MM-DD') AS key,
				to_char(d.bucket, 'Mon DD') AS label,
				COALESCE(c.cnt, 0)::int AS count
			FROM days d
			LEFT JOIN counts c ON c.bucket = d.bucket
			ORDER BY d.bucket ASC`
	}

	rows, err := s.pool.Query(ctx, query, geoArgs...)
	if err != nil {
		return out, err
	}
	defer rows.Close()

	items := make([]TimeBucketCount, 0, 32)
	total := 0
	peak := 0
	for rows.Next() {
		var item TimeBucketCount
		if err := rows.Scan(&item.Key, &item.Label, &item.Count); err != nil {
			return out, err
		}
		items = append(items, item)
		total += item.Count
		if item.Count > peak {
			peak = item.Count
		}
	}
	if err := rows.Err(); err != nil {
		return out, err
	}

	out.Items = items
	out.Total = total
	out.Peak = peak
	if len(items) > 0 {
		out.Average = float64(total) / float64(len(items))
	}
	return out, nil
}

func (s *LeadStore) ListCountries(ctx context.Context, filter GeoFilter) ([]NamedCount, error) {
	where, args := filter.whereSQL("")
	return namedCountsArgs(ctx, s.pool, `
		SELECT `+normalizedCountrySQL("country")+`, COUNT(*)::int
		FROM "Lead"
		`+where+`
		GROUP BY 1
		ORDER BY 2 DESC, 1 ASC`, args...)
}

func (s *LeadStore) ListCities(ctx context.Context, filter GeoFilter) ([]NamedCount, error) {
	where, args := filter.whereSQL("")
	return namedCountsArgs(ctx, s.pool, `
		SELECT
			CASE
				WHEN city IS NULL OR BTRIM(city) = '' OR LOWER(BTRIM(city)) = 'unknown' THEN 'Unknown'
				ELSE BTRIM(city)
			END AS city_name,
			COUNT(*)::int
		FROM "Lead"
		`+where+`
		GROUP BY 1
		ORDER BY 2 DESC, 1 ASC
		LIMIT 500`, args...)
}

func (s *LeadStore) teamLeadCounts(ctx context.Context, params LeadListParams) ([]TeamLeadCount, error) {
	where, args := leadScopeWhere(params, true)
	rows, err := s.pool.Query(ctx, `
		SELECT
			COALESCE(t.id, '') AS team_id,
			COALESCE(NULLIF(BTRIM(t.name), ''), 'Unassigned') AS team_name,
			COUNT(*)::int AS lead_count
		FROM "Lead" l
		LEFT JOIN "Team" t ON t.id = l."teamId"
		`+where+`
		GROUP BY t.id, t.name
		ORDER BY lead_count DESC, team_name ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TeamLeadCount, 0, 32)
	for rows.Next() {
		var item TeamLeadCount
		if err := rows.Scan(&item.ID, &item.Name, &item.Count); err != nil {
			return nil, err
		}
		if item.ID == "" {
			item.Name = "Unassigned"
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *LeadStore) salesExecLeadCounts(ctx context.Context, filter GeoFilter) ([]TeamLeadCount, error) {
	where, args := filter.whereSQL("l.")
	rows, err := s.pool.Query(ctx, `
		SELECT
			COALESCE(u.id, '') AS exec_id,
			COALESCE(NULLIF(BTRIM(u.name), ''), 'Unassigned') AS exec_name,
			COUNT(*)::int AS lead_count
		FROM "Lead" l
		LEFT JOIN "User" u ON u.id = l."assignedSalesExecId"
		`+where+`
		GROUP BY u.id, u.name
		ORDER BY lead_count DESC, exec_name ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]TeamLeadCount, 0, 64)
	for rows.Next() {
		var item TeamLeadCount
		if err := rows.Scan(&item.ID, &item.Name, &item.Count); err != nil {
			return nil, err
		}
		if item.ID == "" {
			item.Name = "Unassigned"
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *LeadStore) analystLeadStats(ctx context.Context, filter GeoFilter) ([]AnalystLeadStats, error) {
	where, args := filter.whereSQL("l.")
	rows, err := s.pool.Query(ctx, `
		SELECT
			COALESCE(u.id, '') AS analyst_id,
			COALESCE(NULLIF(BTRIM(u.name), ''), 'Unassigned') AS analyst_name,
			COALESCE(NULLIF(BTRIM(u.email), ''), '—') AS analyst_email,
			COUNT(*)::int AS total,
			COUNT(*) FILTER (
				WHERE l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CALL', 'QUALIFIED_CHAT')
			)::int AS qualified,
			COUNT(*) FILTER (WHERE l."qualificationStatus" = 'NOT_QUALIFIED')::int AS not_qualified,
			COUNT(*) FILTER (WHERE l."qualificationStatus" = 'IRRELEVANT')::int AS irrelevant
		FROM "Lead" l
		LEFT JOIN "User" u ON u.id = l."createdById"
		`+where+`
		GROUP BY u.id, u.name, u.email
		ORDER BY total DESC, analyst_name ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]AnalystLeadStats, 0, 32)
	for rows.Next() {
		var item AnalystLeadStats
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Email,
			&item.Total, &item.Qualified, &item.NotQualified, &item.Irrelevant,
		); err != nil {
			return nil, err
		}
		if item.ID == "" {
			item.Name = "Unassigned"
			if item.Email == "" {
				item.Email = "—"
			}
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *LeadStore) salesExecOutcomes(ctx context.Context, filter GeoFilter) ([]SalesExecOutcome, error) {
	where, args := filter.whereSQL("l.")
	rows, err := s.pool.Query(ctx, `
		SELECT
			COALESCE(u.id, '') AS exec_id,
			CASE
				WHEN u.id IS NULL THEN 'Unassigned'
				ELSE COALESCE(NULLIF(BTRIM(u.name), ''), 'Unnamed')
			END AS exec_name,
			COUNT(*)::int AS assigned,
			COUNT(*) FILTER (WHERE l."salesStage" = 'WITH_EXECUTIVE')::int AS with_rep,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::int AS won,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_LOST')::int AS lost
		FROM "Lead" l
		LEFT JOIN "User" u ON u.id = l."assignedSalesExecId"
		`+where+`
		GROUP BY u.id, u.name
		ORDER BY
			CASE WHEN u.id IS NULL THEN 0 ELSE 1 END,
			assigned DESC,
			exec_name ASC`, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]SalesExecOutcome, 0, 128)
	for rows.Next() {
		var item SalesExecOutcome
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Assigned, &item.WithRep, &item.Won, &item.Lost,
		); err != nil {
			return nil, err
		}
		if item.ID == "" {
			item.Name = "Unassigned"
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *LeadStore) qualificationReasons(ctx context.Context, filter GeoFilter) ([]StatusReasonCount, error) {
	page, err := s.paginatedQualificationReasons(ctx, LeadListParams{
		Country:     filter.Country,
		City:        filter.City,
		AnalystID:   filter.CreatedByID,
		TeamID:      filter.TeamID,
		SalesExecID: filter.SalesExecID,
	}, "", 0, maxSummaryBucketLimit)
	if err != nil {
		return nil, err
	}
	items, _ := page.Items.([]StatusReasonCount)
	return items, nil
}

// rawQualificationReasonSQL extracts the free-text reason from Lead.notes.
// Kept as raw text (trimmed, length-capped) so dashboard counts match what
// analysts wrote — no taxonomy collapsing into "Other".
const rawQualificationReasonSQL = `
	CASE
		WHEN l.notes IS NULL OR BTRIM(l.notes) = '' THEN 'No reason recorded'
		WHEN l.notes ~* 'qualification\s+reason\s*:' THEN
			NULLIF(
				LEFT(BTRIM(SUBSTRING(l.notes FROM '(?i)qualification\s+reason\s*:\s*([^\n]+)')), 200),
				''
			)
		ELSE
			NULLIF(LEFT(BTRIM(SPLIT_PART(l.notes, E'\n', 1)), 200), '')
	END
`

func (s *LeadStore) SummaryBuckets(
	ctx context.Context,
	dimension string,
	params LeadListParams,
	status string,
	offset, limit int,
) (SummaryBucketPage, error) {
	limit = clampSummaryBucketLimit(limit)
	offset = clampSummaryBucketOffset(offset)
	dimension = strings.ToLower(strings.TrimSpace(dimension))

	// Other mix queries still use geo+creator+team; reasons use full lead scope.
	geo := GeoFilter{
		Country:     params.Country,
		City:        params.City,
		CreatedByID: params.AnalystID,
		TeamID:      params.TeamID,
		SalesExecID: params.SalesExecID,
	}

	switch dimension {
	case "reasons", "qualificationreasons", "qualification-reasons":
		return s.paginatedQualificationReasons(ctx, params, status, offset, limit)
	case "portal", "website":
		return s.paginatedAttributionMix(ctx, geo, `"portalWebsite"`, "portal", offset, limit)
	case "metaprofile", "meta-profile", "meta_profile":
		return s.paginatedAttributionMix(ctx, geo, `"sourceMetaProfileName"`, "metaProfile", offset, limit)
	case "source":
		return s.paginatedAttributionMix(ctx, geo, `source`, "source", offset, limit)
	case "analyst", "analysts":
		return s.paginatedAnalystLeadStats(ctx, geo, offset, limit)
	case "salesexec", "sales-exec", "sales_exec", "salesexecoutcomes":
		return s.paginatedSalesExecOutcomes(ctx, geo, offset, limit)
	default:
		return SummaryBucketPage{}, fmt.Errorf("unknown summary dimension %q", dimension)
	}
}

func (s *LeadStore) paginatedQualificationReasons(
	ctx context.Context,
	params LeadListParams,
	status string,
	offset, limit int,
) (SummaryBucketPage, error) {
	status = strings.ToUpper(strings.TrimSpace(status))
	scopeSQL, args := leadScopeWhere(params, true)
	statusPool := `l."qualificationStatus" IN ('IRRELEVANT', 'NOT_QUALIFIED')`
	if scopeSQL == "" {
		scopeSQL = "WHERE " + statusPool
	} else {
		scopeSQL = scopeSQL + " AND " + statusPool
	}

	statusFilter := ""
	if status == "IRRELEVANT" || status == "NOT_QUALIFIED" {
		args = append(args, status)
		statusFilter = fmt.Sprintf(` AND status = $%d`, len(args))
	} else {
		status = ""
	}

	metaQuery := `
		WITH raw AS (
			SELECT
				l."qualificationStatus" AS status,
				COALESCE(` + rawQualificationReasonSQL + `, 'No reason recorded') AS reason
			FROM "Lead" l
			` + scopeSQL + `
		)
		SELECT COUNT(*)::int, COALESCE(SUM(reason_count), 0)::int
		FROM (
			SELECT status, reason, COUNT(*)::int AS reason_count
			FROM raw
			WHERE TRUE` + statusFilter + `
			GROUP BY status, reason
		) buckets`

	var bucketCount, leadTotal int
	if err := s.pool.QueryRow(ctx, metaQuery, args...).Scan(&bucketCount, &leadTotal); err != nil {
		return SummaryBucketPage{}, err
	}

	base := len(args)
	pageArgs := append(append([]any{}, args...), limit+1, offset)
	limitPos := base + 1
	offsetPos := base + 2

	rows, err := s.pool.Query(ctx, `
		WITH raw AS (
			SELECT
				l."qualificationStatus" AS status,
				COALESCE(`+rawQualificationReasonSQL+`, 'No reason recorded') AS reason
			FROM "Lead" l
			`+scopeSQL+`
		)
		SELECT status, reason, COUNT(*)::int AS reason_count
		FROM raw
		WHERE TRUE`+statusFilter+`
		GROUP BY status, reason
		ORDER BY reason_count DESC, reason ASC, status ASC
		LIMIT $`+strconv.Itoa(limitPos)+` OFFSET $`+strconv.Itoa(offsetPos), pageArgs...)
	if err != nil {
		return SummaryBucketPage{}, err
	}
	defer rows.Close()

	out := make([]StatusReasonCount, 0, limit)
	for rows.Next() {
		var item StatusReasonCount
		if err := rows.Scan(&item.Status, &item.Reason, &item.Count); err != nil {
			return SummaryBucketPage{}, err
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return SummaryBucketPage{}, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	return SummaryBucketPage{
		Dimension:   "reasons",
		Offset:      offset,
		Limit:       limit,
		HasMore:     hasMore,
		BucketCount: bucketCount,
		LeadTotal:   leadTotal,
		Items:       out,
	}, nil
}

func (s *LeadStore) paginatedAttributionMix(
	ctx context.Context,
	filter GeoFilter,
	columnSQL, dimension string,
	offset, limit int,
) (SummaryBucketPage, error) {
	where, args := filter.whereSQL("")

	metaQuery := `
		SELECT COUNT(*)::int,
		       COALESCE(SUM(total), 0)::int,
		       COALESCE(SUM(won), 0)::int
		FROM (
			SELECT
				COALESCE(NULLIF(BTRIM(` + columnSQL + `), ''), 'Unassigned') AS name,
				COUNT(*)::int AS total,
				COUNT(*) FILTER (WHERE "salesStage" = 'CLOSED_WON')::int AS won
			FROM "Lead"
			` + where + `
			GROUP BY 1
		) buckets`
	var bucketCount, leadTotal, wonTotal int
	if err := s.pool.QueryRow(ctx, metaQuery, args...).Scan(&bucketCount, &leadTotal, &wonTotal); err != nil {
		return SummaryBucketPage{}, err
	}

	base := len(args)
	pageArgs := append(append([]any{}, args...), limit+1, offset)
	limitPos := base + 1
	offsetPos := base + 2

	rows, err := s.pool.Query(ctx, `
		SELECT
			COALESCE(NULLIF(BTRIM(`+columnSQL+`), ''), 'Unassigned') AS name,
			COUNT(*)::int AS total,
			COUNT(*) FILTER (WHERE "salesStage" = 'CLOSED_WON')::int AS won
		FROM "Lead"
		`+where+`
		GROUP BY 1
		ORDER BY 2 DESC, 1 ASC
		LIMIT $`+strconv.Itoa(limitPos)+` OFFSET $`+strconv.Itoa(offsetPos), pageArgs...)
	if err != nil {
		return SummaryBucketPage{}, err
	}
	defer rows.Close()

	out := make([]AttributionStats, 0, limit)
	for rows.Next() {
		var item AttributionStats
		if err := rows.Scan(&item.Name, &item.Total, &item.Won); err != nil {
			return SummaryBucketPage{}, err
		}
		if item.Total > 0 {
			item.Conversion = float64(item.Won) * 100 / float64(item.Total)
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return SummaryBucketPage{}, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	return SummaryBucketPage{
		Dimension:   dimension,
		Offset:      offset,
		Limit:       limit,
		HasMore:     hasMore,
		BucketCount: bucketCount,
		LeadTotal:   leadTotal,
		WonTotal:    wonTotal,
		Items:       out,
	}, nil
}

func (s *LeadStore) paginatedAnalystLeadStats(
	ctx context.Context,
	filter GeoFilter,
	offset, limit int,
) (SummaryBucketPage, error) {
	where, args := filter.whereSQL("l.")

	metaQuery := `
		SELECT COUNT(*)::int, COALESCE(SUM(total), 0)::int
		FROM (
			SELECT
				COALESCE(u.id, '') AS analyst_id,
				COUNT(*)::int AS total
			FROM "Lead" l
			LEFT JOIN "User" u ON u.id = l."createdById"
			` + where + `
			GROUP BY u.id
		) buckets`
	var bucketCount, leadTotal int
	if err := s.pool.QueryRow(ctx, metaQuery, args...).Scan(&bucketCount, &leadTotal); err != nil {
		return SummaryBucketPage{}, err
	}

	base := len(args)
	pageArgs := append(append([]any{}, args...), limit+1, offset)
	limitPos := base + 1
	offsetPos := base + 2

	rows, err := s.pool.Query(ctx, `
		SELECT
			COALESCE(u.id, '') AS analyst_id,
			COALESCE(NULLIF(BTRIM(u.name), ''), 'Unassigned') AS analyst_name,
			COALESCE(NULLIF(BTRIM(u.email), ''), '—') AS analyst_email,
			COUNT(*)::int AS total,
			COUNT(*) FILTER (
				WHERE l."qualificationStatus" IN ('QUALIFIED', 'QUALIFIED_CALL', 'QUALIFIED_CHAT')
			)::int AS qualified,
			COUNT(*) FILTER (WHERE l."qualificationStatus" = 'NOT_QUALIFIED')::int AS not_qualified,
			COUNT(*) FILTER (WHERE l."qualificationStatus" = 'IRRELEVANT')::int AS irrelevant
		FROM "Lead" l
		LEFT JOIN "User" u ON u.id = l."createdById"
		`+where+`
		GROUP BY u.id, u.name, u.email
		ORDER BY total DESC, analyst_name ASC
		LIMIT $`+strconv.Itoa(limitPos)+` OFFSET $`+strconv.Itoa(offsetPos), pageArgs...)
	if err != nil {
		return SummaryBucketPage{}, err
	}
	defer rows.Close()

	out := make([]AnalystLeadStats, 0, limit)
	for rows.Next() {
		var item AnalystLeadStats
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Email,
			&item.Total, &item.Qualified, &item.NotQualified, &item.Irrelevant,
		); err != nil {
			return SummaryBucketPage{}, err
		}
		if item.ID == "" {
			item.Name = "Unassigned"
			if item.Email == "" {
				item.Email = "—"
			}
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return SummaryBucketPage{}, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	return SummaryBucketPage{
		Dimension:   "analyst",
		Offset:      offset,
		Limit:       limit,
		HasMore:     hasMore,
		BucketCount: bucketCount,
		LeadTotal:   leadTotal,
		Items:       out,
	}, nil
}

func (s *LeadStore) paginatedSalesExecOutcomes(
	ctx context.Context,
	filter GeoFilter,
	offset, limit int,
) (SummaryBucketPage, error) {
	where, args := filter.whereSQL("l.")

	metaQuery := `
		SELECT COUNT(*)::int, COALESCE(SUM(assigned), 0)::int
		FROM (
			SELECT
				COALESCE(u.id, '') AS exec_id,
				COUNT(*)::int AS assigned
			FROM "Lead" l
			LEFT JOIN "User" u ON u.id = l."assignedSalesExecId"
			` + where + `
			GROUP BY u.id
		) buckets`
	var bucketCount, leadTotal int
	if err := s.pool.QueryRow(ctx, metaQuery, args...).Scan(&bucketCount, &leadTotal); err != nil {
		return SummaryBucketPage{}, err
	}

	base := len(args)
	pageArgs := append(append([]any{}, args...), limit+1, offset)
	limitPos := base + 1
	offsetPos := base + 2

	rows, err := s.pool.Query(ctx, `
		SELECT
			COALESCE(u.id, '') AS exec_id,
			CASE
				WHEN u.id IS NULL THEN 'Unassigned'
				ELSE COALESCE(NULLIF(BTRIM(u.name), ''), 'Unnamed')
			END AS exec_name,
			COUNT(*)::int AS assigned,
			COUNT(*) FILTER (WHERE l."salesStage" = 'WITH_EXECUTIVE')::int AS with_rep,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_WON')::int AS won,
			COUNT(*) FILTER (WHERE l."salesStage" = 'CLOSED_LOST')::int AS lost
		FROM "Lead" l
		LEFT JOIN "User" u ON u.id = l."assignedSalesExecId"
		`+where+`
		GROUP BY u.id, u.name
		ORDER BY assigned DESC, exec_name ASC
		LIMIT $`+strconv.Itoa(limitPos)+` OFFSET $`+strconv.Itoa(offsetPos), pageArgs...)
	if err != nil {
		return SummaryBucketPage{}, err
	}
	defer rows.Close()

	out := make([]SalesExecOutcome, 0, limit)
	for rows.Next() {
		var item SalesExecOutcome
		if err := rows.Scan(
			&item.ID, &item.Name, &item.Assigned, &item.WithRep, &item.Won, &item.Lost,
		); err != nil {
			return SummaryBucketPage{}, err
		}
		if item.ID == "" {
			item.Name = "Unassigned"
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return SummaryBucketPage{}, err
	}

	hasMore := len(out) > limit
	if hasMore {
		out = out[:limit]
	}

	return SummaryBucketPage{
		Dimension:   "salesExec",
		Offset:      offset,
		Limit:       limit,
		HasMore:     hasMore,
		BucketCount: bucketCount,
		LeadTotal:   leadTotal,
		Items:       out,
	}, nil
}

type CreateLeadInput struct {
	FullName               string
	Email                  *string
	Phone                  *string
	Country                *string
	City                   *string
	PortalWebsite          *string
	Source                 string
	SourceMetaProfileName  *string
	Language               *string
	ClientProfile          *string
	QualificationStatus    string
	LeadScore              *int
	CreatedAt              *time.Time
	Notes                  *string
	FirstClientMessageAt   *time.Time
	FirstAgentMessageAt    *time.Time
	FirstResponseMinutes   *int
	FirstResponseProofPath *string
	CreatedByID            string
}

var allowedLeadSources = map[string]struct{}{
	"Meta WhatsApp":           {},
	"Meta Messenger":          {},
	"Website WhatsApp":        {},
	"Meta Lead Form":          {},
	"Website Download Form":   {},
	"G.WhatsApp(CAM/CWA/CRW)": {},
	"Google LeadForm":         {},
}

var allowedQualifications = map[string]struct{}{
	"QUALIFIED":      {},
	"QUALIFIED_CHAT": {},
	"QUALIFIED_CALL": {},
	"NOT_QUALIFIED":  {},
	"IRRELEVANT":     {},
}

func isAssignableQualification(status string) bool {
	switch strings.TrimSpace(status) {
	case "QUALIFIED", "QUALIFIED_CHAT", "QUALIFIED_CALL":
		return true
	default:
		return false
	}
}

func (s *LeadStore) UpdateQualificationStatus(ctx context.Context, id, status string) (string, error) {
	id = strings.TrimSpace(id)
	status = strings.TrimSpace(status)
	if id == "" {
		return "", fmt.Errorf("lead id is required")
	}
	if _, ok := allowedQualifications[status]; !ok {
		return "", fmt.Errorf("invalid qualification status")
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Lead"
		SET "qualificationStatus" = $2, "updatedAt" = $3
		WHERE id = $1`, id, status, time.Now().UTC())
	if err != nil {
		return "", err
	}
	if tag.RowsAffected() == 0 {
		return "", fmt.Errorf("lead not found")
	}
	return status, nil
}

func (s *LeadStore) defaultCreatorID(ctx context.Context) (string, error) {
	var id string
	err := s.pool.QueryRow(ctx, `
		SELECT id FROM "User"
		WHERE role = 'SUPERADMIN'
		ORDER BY "createdAt" ASC
		LIMIT 1`).Scan(&id)
	return id, err
}

func contactPhoneDigits(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	return b.String()
}

// LeadContactMatch is a duplicate hit on phone + portal + source.
type LeadContactMatch struct {
	ID            string   `json:"id"`
	LeadName      string   `json:"leadName"`
	TeamName      *string  `json:"teamName"`
	MatchedOn     string   `json:"matchedOn"`
	MatchedFields []string `json:"matchedFields"`
}

// PhonePresence is the add-lead phone lookup: team plus portals/sources
// already used with that number on the platform.
type PhonePresence struct {
	ID      string
	LeadName string
	TeamName *string
	Portals []string
	Sources []string
}

// FindLeadByPhone returns any lead with the same phone digits (team label
// for the add-lead form). excludeID skips that lead (edit mode).
func (s *LeadStore) FindLeadByPhone(
	ctx context.Context,
	phone, excludeID string,
) (*LeadContactMatch, error) {
	presence, err := s.FindPhonePresence(ctx, phone, excludeID)
	if err != nil || presence == nil {
		return nil, err
	}
	return &LeadContactMatch{
		ID:            presence.ID,
		LeadName:      presence.LeadName,
		TeamName:      presence.TeamName,
		MatchedOn:     "phone",
		MatchedFields: []string{"phone"},
	}, nil
}

// FindPhonePresence lists team + distinct portals/sources for a phone number.
func (s *LeadStore) FindPhonePresence(
	ctx context.Context,
	phone, excludeID string,
) (*PhonePresence, error) {
	digits := contactPhoneDigits(phone)
	if len(digits) < 5 {
		return nil, nil
	}
	exclude := strings.TrimSpace(excludeID)

	rows, err := s.pool.Query(ctx, `
		SELECT
			l.id,
			l."leadName",
			NULLIF(BTRIM(COALESCE(t.name, '')), ''),
			BTRIM(COALESCE(l."portalWebsite", '')),
			BTRIM(COALESCE(l.source, ''))
		FROM "Lead" l
		LEFT JOIN "Team" t ON t.id = l."teamId"
		WHERE ($2 = '' OR l.id <> $2)
		  AND l.phone IS NOT NULL
		  AND regexp_replace(COALESCE(l.phone, ''), '[^0-9]', '', 'g') = $1
		ORDER BY l."updatedAt" DESC NULLS LAST
		LIMIT 50`,
		digits, exclude,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var presence *PhonePresence
	portalSet := map[string]struct{}{}
	sourceSet := map[string]struct{}{}
	for rows.Next() {
		var (
			id, name, portal, source string
			teamName                 *string
		)
		if err := rows.Scan(&id, &name, &teamName, &portal, &source); err != nil {
			return nil, err
		}
		if presence == nil {
			presence = &PhonePresence{
				ID:       id,
				LeadName: name,
				TeamName: teamName,
			}
		}
		if portal != "" {
			if _, ok := portalSet[portal]; !ok {
				portalSet[portal] = struct{}{}
				presence.Portals = append(presence.Portals, portal)
			}
		}
		if source != "" {
			if _, ok := sourceSet[source]; !ok {
				sourceSet[source] = struct{}{}
				presence.Sources = append(presence.Sources, source)
			}
		}
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return presence, nil
}

// FindDuplicateByPhonePortalSource finds another lead with the same phone
// digits, portal website, and lead source. Kept for diagnostics; create/update
// no longer block on this. excludeID skips that lead (edit mode).
func (s *LeadStore) FindDuplicateByPhonePortalSource(
	ctx context.Context,
	phone, portalWebsite, source, excludeID string,
) (*LeadContactMatch, error) {
	digits := contactPhoneDigits(phone)
	if len(digits) < 5 {
		return nil, nil
	}
	sourceNorm := strings.TrimSpace(source)
	if sourceNorm == "" {
		return nil, nil
	}
	portalNorm := strings.ToLower(strings.TrimSpace(portalWebsite))
	exclude := strings.TrimSpace(excludeID)

	var (
		id, name string
		teamName *string
	)
	err := s.pool.QueryRow(ctx, `
		SELECT
			l.id,
			l."leadName",
			NULLIF(BTRIM(COALESCE(t.name, '')), '')
		FROM "Lead" l
		LEFT JOIN "Team" t ON t.id = l."teamId"
		WHERE ($4 = '' OR l.id <> $4)
		  AND l.phone IS NOT NULL
		  AND regexp_replace(COALESCE(l.phone, ''), '[^0-9]', '', 'g') = $1
		  AND BTRIM(COALESCE(l.source, '')) = $2
		  AND lower(BTRIM(COALESCE(l."portalWebsite", ''))) = $3
		LIMIT 1`,
		digits, sourceNorm, portalNorm, exclude,
	).Scan(&id, &name, &teamName)
	if err == pgx.ErrNoRows {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	return &LeadContactMatch{
		ID:            id,
		LeadName:      name,
		TeamName:      teamName,
		MatchedOn:     "phone+portal+source",
		MatchedFields: []string{"phone", "portalWebsite", "source"},
	}, nil
}

func (s *LeadStore) Create(ctx context.Context, in CreateLeadInput) (string, error) {
	if n := utf8.RuneCountInString(strings.TrimSpace(in.FullName)); n > 200 {
		return "", fmt.Errorf("full name must be at most 200 characters")
	}
	if _, ok := allowedLeadSources[in.Source]; !ok {
		return "", fmt.Errorf("invalid source")
	}
	if _, ok := allowedQualifications[in.QualificationStatus]; !ok {
		return "", fmt.Errorf("invalid qualification status")
	}
	if in.LeadScore != nil && (*in.LeadScore < 0 || *in.LeadScore > 100) {
		return "", fmt.Errorf("lead score must be between 0 and 100")
	}
	if in.FirstResponseMinutes != nil && (*in.FirstResponseMinutes < 0 || *in.FirstResponseMinutes > 7*24*60) {
		return "", fmt.Errorf("first response duration must be between 0 minutes and 7 days")
	}

	createdBy := strings.TrimSpace(in.CreatedByID)
	if createdBy == "" {
		id, err := s.defaultCreatorID(ctx)
		if err != nil {
			return "", fmt.Errorf("no creator available: %w", err)
		}
		createdBy = id
	}

	now := time.Now().UTC()
	createdAt := now
	if in.CreatedAt != nil {
		createdAt = in.CreatedAt.UTC()
	}

	id := uuid.NewString()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return "", err
	}
	defer tx.Rollback(ctx)

	proofPath := optionalProofPath(in.FirstResponseProofPath)

	_, err = tx.Exec(ctx, `
		INSERT INTO "Lead" (
			id, "leadName", phone, "leadEmail", country, city,
			source, "sourceMetaProfileName", notes,
			"qualificationStatus", "leadScore", "salesStage",
			"createdById", "internalReassignCount",
			"createdAt", "updatedAt", "dealCurrency",
			"portalWebsite", language, "clientProfile",
			"firstClientMessageAt", "firstAgentMessageAt",
			"firstResponseMinutes", "firstResponseProofPath"
		) VALUES (
			$1,$2,$3,$4,$5,$6,
			$7,$8,$9,
			$10,$11,'PRE_SALES',
			$12,0,
			$13,$14,'AUD',
			$15,$16,$17,
			$18,$19,
			$20,$21
		)`,
		id,
		strings.TrimSpace(in.FullName),
		in.Phone,
		in.Email,
		in.Country,
		in.City,
		in.Source,
		in.SourceMetaProfileName,
		in.Notes,
		in.QualificationStatus,
		in.LeadScore,
		createdBy,
		createdAt,
		now,
		in.PortalWebsite,
		in.Language,
		in.ClientProfile,
		in.FirstClientMessageAt,
		in.FirstAgentMessageAt,
		in.FirstResponseMinutes,
		proofPath,
	)
	if err != nil {
		return "", err
	}

	detail := "Created via LeadFlow UI"
	var actorName string
	if err := tx.QueryRow(ctx, `SELECT name FROM "User" WHERE id = $1`, createdBy).Scan(&actorName); err == nil && actorName != "" {
		detail = "Created by " + actorName
	}

	_, err = tx.Exec(ctx, `
		INSERT INTO "LeadHandoffLog" (id, "createdAt", "leadId", action, "actorId", detail)
		VALUES ($1, $2, $3, 'LEAD_CREATED', $4, $5)`,
		uuid.NewString(), createdAt, id, createdBy, detail,
	)
	if err != nil {
		return "", err
	}

	if err := tx.Commit(ctx); err != nil {
		return "", err
	}
	return id, nil
}

// LeadDetail is the editable lead payload for the add/edit drawer.
type LeadDetail struct {
	ID                     string   `json:"id"`
	FullName               string   `json:"fullName"`
	Email                  *string  `json:"email"`
	Phone                  *string  `json:"phone"`
	Country                *string  `json:"country"`
	City                   *string  `json:"city"`
	PortalWebsite          *string  `json:"portalWebsite"`
	Source                 string   `json:"source"`
	SourceMetaProfileName  *string  `json:"facebookProfile"`
	Language               *string  `json:"language"`
	ClientProfile          *string  `json:"clientProfile"`
	QualificationStatus    string   `json:"qualificationStatus"`
	LeadScore              *int     `json:"leadScore"`
	CreatedAt              string   `json:"createdAt"`
	Notes                  *string  `json:"notes"`
	FirstClientMessageAt   *string  `json:"firstClientMessageAt"`
	FirstAgentMessageAt    *string  `json:"firstAgentMessageAt"`
	FirstResponseMinutes   *int     `json:"firstResponseMinutes"`
	FirstResponseProofPath *string  `json:"firstResponseProofPath"`
	SalesStage             string   `json:"salesStage"`
	SalesStageLabel        string   `json:"salesStageLabel"`
	InitialPayment         *float64 `json:"initialPayment"`
	ClosedRevenue          *float64 `json:"closedRevenue"`
	EstimatedDealValue     *float64 `json:"estimatedDealValue"`
	DealValue              *float64 `json:"dealValue"`
	DealValueDisplay       string   `json:"dealValueDisplay"`
	DealCurrency           string   `json:"dealCurrency"`
	ExecutiveNotes         *string  `json:"executiveNotes"`
	Closed                 string   `json:"closed"`
	NotAppropriate         bool     `json:"notAppropriate"`
	NotAppropriateReason   *string  `json:"notAppropriateReason"`
	NotAppropriateAt       *string  `json:"notAppropriateAt,omitempty"`
}

func optionalProofPath(raw *string) *string {
	if raw == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

// SalesOutcomeInput is the SE-editable outcome payload.
type SalesOutcomeInput struct {
	SalesStage     string
	InitialPayment *float64
	ClosedRevenue  *float64
	ExecutiveNotes *string
	HasStage       bool
	HasPayment     bool
	HasRevenue     bool
	HasNotes       bool
}

func (s *LeadStore) GetByID(ctx context.Context, id string) (LeadDetail, error) {
	var out LeadDetail
	var createdAt time.Time
	var closedAt *time.Time
	var notAppropriateAt *time.Time
	var proofPath *string
	var clientAt, agentAt *time.Time
	err := s.pool.QueryRow(ctx, `
		SELECT
			id,
			"leadName",
			"leadEmail",
			phone,
			country,
			city,
			"portalWebsite",
			source,
			"sourceMetaProfileName",
			language,
			"clientProfile",
			"qualificationStatus",
			"leadScore",
			"createdAt",
			notes,
			"firstClientMessageAt",
			"firstAgentMessageAt",
			"firstResponseMinutes",
			"firstResponseProofPath",
			"salesStage",
			"initialPayment",
			"closedRevenue",
			"estimatedDealValue",
			"dealCurrency",
			"lostNotes",
			"closedAt",
			"notAppropriate",
			"notAppropriateReason",
			"notAppropriateAt"
		FROM "Lead"
		WHERE id = $1`, id).Scan(
		&out.ID,
		&out.FullName,
		&out.Email,
		&out.Phone,
		&out.Country,
		&out.City,
		&out.PortalWebsite,
		&out.Source,
		&out.SourceMetaProfileName,
		&out.Language,
		&out.ClientProfile,
		&out.QualificationStatus,
		&out.LeadScore,
		&createdAt,
		&out.Notes,
		&clientAt,
		&agentAt,
		&out.FirstResponseMinutes,
		&proofPath,
		&out.SalesStage,
		&out.InitialPayment,
		&out.ClosedRevenue,
		&out.EstimatedDealValue,
		&out.DealCurrency,
		&out.ExecutiveNotes,
		&closedAt,
		&out.NotAppropriate,
		&out.NotAppropriateReason,
		&notAppropriateAt,
	)
	if err != nil {
		return LeadDetail{}, err
	}
	out.CreatedAt = createdAt.In(time.Local).Format("2006-01-02")
	out.SalesStageLabel = salesStageDisplay(out.SalesStage)
	out.Closed = closedLabel(closedAt, out.SalesStage)
	if strings.TrimSpace(out.DealCurrency) == "" {
		out.DealCurrency = "AUD"
	}
	if clientAt != nil {
		formatted := clientAt.In(time.Local).Format("2006-01-02T15:04")
		out.FirstClientMessageAt = &formatted
	}
	if agentAt != nil {
		formatted := agentAt.In(time.Local).Format("2006-01-02T15:04")
		out.FirstAgentMessageAt = &formatted
	}
	if notAppropriateAt != nil {
		formatted := notAppropriateAt.UTC().Format(time.RFC3339)
		out.NotAppropriateAt = &formatted
	}
	if proofPath != nil && strings.TrimSpace(*proofPath) != "" {
		public := proofPublicPath(proofStoredNameFromPath(*proofPath))
		out.FirstResponseProofPath = &public
	}
	// Closed revenue is the lead's deal value when set.
	if out.ClosedRevenue != nil {
		out.DealValue = out.ClosedRevenue
	} else {
		out.DealValue = out.EstimatedDealValue
	}
	out.DealValueDisplay = formatDealValue(out.DealValue, out.DealCurrency)
	return out, nil
}

func (s *LeadStore) UpdateSalesOutcome(ctx context.Context, id string, in SalesOutcomeInput) (LeadDetail, error) {
	if strings.TrimSpace(id) == "" {
		return LeadDetail{}, fmt.Errorf("lead id is required")
	}
	if !in.HasStage && !in.HasPayment && !in.HasRevenue && !in.HasNotes {
		return s.GetByID(ctx, id)
	}

	var currentStage string
	var currentClosedAt *time.Time
	var existingRevenue *float64
	err := s.pool.QueryRow(ctx, `
		SELECT "salesStage", "closedAt", "closedRevenue" FROM "Lead" WHERE id = $1`, id).Scan(
		&currentStage, &currentClosedAt, &existingRevenue,
	)
	if err != nil {
		return LeadDetail{}, fmt.Errorf("lead not found")
	}

	stage := currentStage
	if in.HasStage {
		stage = strings.TrimSpace(in.SalesStage)
		if !isSalesOutcomeStage(stage) {
			return LeadDetail{}, fmt.Errorf("invalid sales outcome")
		}
	}

	if in.HasPayment && in.InitialPayment != nil && *in.InitialPayment < 0 {
		return LeadDetail{}, fmt.Errorf("initial payment cannot be negative")
	}
	if in.HasRevenue && in.ClosedRevenue != nil && *in.ClosedRevenue < 0 {
		return LeadDetail{}, fmt.Errorf("closed revenue cannot be negative")
	}

	revenue := existingRevenue
	if in.HasRevenue {
		revenue = in.ClosedRevenue
	}
	if stage == "CLOSED_WON" && (revenue == nil || *revenue <= 0) {
		return LeadDetail{}, fmt.Errorf("closed revenue is required when outcome is Closed")
	}

	now := time.Now().UTC()
	setParts := []string{`"updatedAt" = $2`}
	args := []any{id, now}
	next := func(v any) string {
		args = append(args, v)
		return fmt.Sprintf("$%d", len(args))
	}

	if in.HasStage {
		setParts = append(setParts, `"salesStage" = `+next(stage))
		var closedAt any
		switch stage {
		case "CLOSED_WON", "CLOSED_LOST":
			if currentClosedAt != nil {
				closedAt = *currentClosedAt
			} else {
				closedAt = now
			}
		default:
			closedAt = nil
		}
		setParts = append(setParts, `"closedAt" = `+next(closedAt))
	}
	if in.HasPayment {
		setParts = append(setParts, `"initialPayment" = `+next(in.InitialPayment))
	}
	if in.HasRevenue {
		// Closed revenue is the global deal value for this lead.
		setParts = append(setParts, `"closedRevenue" = `+next(in.ClosedRevenue))
		setParts = append(setParts, `"estimatedDealValue" = `+next(in.ClosedRevenue))
		setParts = append(setParts, `"dealCurrency" = `+next("AUD"))
	}
	if in.HasNotes {
		setParts = append(setParts, `"lostNotes" = `+next(in.ExecutiveNotes))
	}

	sql := fmt.Sprintf(`UPDATE "Lead" SET %s WHERE id = $1`, strings.Join(setParts, ", "))
	tag, err := s.pool.Exec(ctx, sql, args...)
	if err != nil {
		return LeadDetail{}, err
	}
	if tag.RowsAffected() == 0 {
		return LeadDetail{}, fmt.Errorf("lead not found")
	}
	return s.GetByID(ctx, id)
}

const (
	notAppropriateReasonMin = 10
	notAppropriateReasonMax = 2000
)

// MarkNotAppropriate flags a lead as not appropriate for sales with a required reason.
// Idempotent when already flagged with the same reason; otherwise rejects re-marking.
func (s *LeadStore) MarkNotAppropriate(ctx context.Context, id, reason, actorID string) (LeadDetail, error) {
	id = strings.TrimSpace(id)
	reason = strings.TrimSpace(reason)
	actorID = strings.TrimSpace(actorID)
	if id == "" {
		return LeadDetail{}, fmt.Errorf("lead id is required")
	}
	if reason == "" {
		return LeadDetail{}, fmt.Errorf("reason is required")
	}
	if len([]rune(reason)) < notAppropriateReasonMin {
		return LeadDetail{}, fmt.Errorf("reason must be at least %d characters", notAppropriateReasonMin)
	}
	if len([]rune(reason)) > notAppropriateReasonMax {
		return LeadDetail{}, fmt.Errorf("reason must be at most %d characters", notAppropriateReasonMax)
	}

	var already bool
	var existingReason *string
	err := s.pool.QueryRow(ctx, `
		SELECT "notAppropriate", "notAppropriateReason"
		FROM "Lead" WHERE id = $1`, id).Scan(&already, &existingReason)
	if err != nil {
		return LeadDetail{}, fmt.Errorf("lead not found")
	}
	if already {
		if existingReason != nil && strings.TrimSpace(*existingReason) == reason {
			return s.GetByID(ctx, id)
		}
		return LeadDetail{}, fmt.Errorf("lead is already marked not appropriate")
	}

	now := time.Now().UTC()
	var byID any
	if actorID != "" {
		byID = actorID
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Lead"
		SET
			"notAppropriate" = TRUE,
			"notAppropriateReason" = $2,
			"notAppropriateAt" = $3::timestamptz,
			"notAppropriateById" = $4,
			"qualificationStatus" = 'IRRELEVANT',
			"updatedAt" = $5::timestamptz
		WHERE id = $1 AND "notAppropriate" = FALSE`,
		id, reason, now, byID, now,
	)
	if err != nil {
		return LeadDetail{}, err
	}
	if tag.RowsAffected() == 0 {
		return LeadDetail{}, fmt.Errorf("lead is already marked not appropriate")
	}
	return s.GetByID(ctx, id)
}

// IsCreatedBy reports whether the lead exists and was created by userID.
func (s *LeadStore) IsCreatedBy(ctx context.Context, leadID, userID string) (bool, error) {
	leadID = strings.TrimSpace(leadID)
	userID = strings.TrimSpace(userID)
	if leadID == "" || userID == "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM "Lead"
			WHERE id = $1 AND "createdById" = $2
		)`, leadID, userID).Scan(&ok)
	return ok, err
}

// IsOnTeam reports whether the lead exists and belongs to teamID.
func (s *LeadStore) IsOnTeam(ctx context.Context, leadID, teamID string) (bool, error) {
	leadID = strings.TrimSpace(leadID)
	teamID = strings.TrimSpace(teamID)
	if leadID == "" || teamID == "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM "Lead"
			WHERE id = $1 AND "teamId" = $2
		)`, leadID, teamID).Scan(&ok)
	return ok, err
}

// IsAssignedToSalesExec reports whether the lead exists and is assigned to salesExecID.
func (s *LeadStore) IsAssignedToSalesExec(ctx context.Context, leadID, salesExecID string) (bool, error) {
	leadID = strings.TrimSpace(leadID)
	salesExecID = strings.TrimSpace(salesExecID)
	if leadID == "" || salesExecID == "" {
		return false, nil
	}
	var ok bool
	err := s.pool.QueryRow(ctx, `
		SELECT EXISTS(
			SELECT 1 FROM "Lead"
			WHERE id = $1 AND "assignedSalesExecId" = $2
		)`, leadID, salesExecID).Scan(&ok)
	return ok, err
}

func (s *LeadStore) Update(ctx context.Context, id string, in CreateLeadInput) error {
	if strings.TrimSpace(id) == "" {
		return fmt.Errorf("lead id is required")
	}
	if n := utf8.RuneCountInString(strings.TrimSpace(in.FullName)); n > 200 {
		return fmt.Errorf("full name must be at most 200 characters")
	}

	var currentSource, currentQual string
	err := s.pool.QueryRow(ctx, `
		SELECT source, "qualificationStatus" FROM "Lead" WHERE id = $1`, id).Scan(
		&currentSource, &currentQual,
	)
	if err != nil {
		return fmt.Errorf("lead not found")
	}

	if _, ok := allowedLeadSources[in.Source]; !ok && in.Source != currentSource {
		return fmt.Errorf("invalid source")
	}
	if _, ok := allowedQualifications[in.QualificationStatus]; !ok && in.QualificationStatus != currentQual {
		return fmt.Errorf("invalid qualification status")
	}
	if in.LeadScore != nil && (*in.LeadScore < 0 || *in.LeadScore > 100) {
		return fmt.Errorf("lead score must be between 0 and 100")
	}
	if in.FirstResponseMinutes != nil && (*in.FirstResponseMinutes < 0 || *in.FirstResponseMinutes > 7*24*60) {
		return fmt.Errorf("first response duration must be between 0 minutes and 7 days")
	}

	now := time.Now().UTC()
	var createdAt any
	if in.CreatedAt != nil {
		createdAt = in.CreatedAt.UTC()
	} else {
		createdAt = nil
	}

	proofPath := optionalProofPath(in.FirstResponseProofPath)
	// Explicit empty string from the client clears stored proof.
	if in.FirstResponseProofPath != nil && strings.TrimSpace(*in.FirstResponseProofPath) == "" {
		proofPath = nil
	}

	tag, err := s.pool.Exec(ctx, `
		UPDATE "Lead" SET
			"leadName" = $2,
			"leadEmail" = $3,
			phone = $4,
			country = $5,
			city = $6,
			"portalWebsite" = $7,
			source = $8,
			"sourceMetaProfileName" = $9,
			language = $10,
			"clientProfile" = $11,
			"qualificationStatus" = $12,
			"leadScore" = $13,
			notes = $14,
			"firstClientMessageAt" = $15,
			"firstAgentMessageAt" = $16,
			"firstResponseMinutes" = $17,
			"firstResponseProofPath" = $18,
			"createdAt" = COALESCE($19, "createdAt"),
			"updatedAt" = $20
		WHERE id = $1`,
		id,
		strings.TrimSpace(in.FullName),
		in.Email,
		in.Phone,
		in.Country,
		in.City,
		in.PortalWebsite,
		in.Source,
		in.SourceMetaProfileName,
		in.Language,
		in.ClientProfile,
		in.QualificationStatus,
		in.LeadScore,
		in.Notes,
		in.FirstClientMessageAt,
		in.FirstAgentMessageAt,
		in.FirstResponseMinutes,
		proofPath,
		createdAt,
		now,
	)
	if err != nil {
		return err
	}
	if tag.RowsAffected() == 0 {
		return fmt.Errorf("lead not found")
	}
	return nil
}
