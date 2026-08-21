package main

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/go-pdf/fpdf"
	"github.com/jackc/pgx/v5"
)

type LeadExportRow struct {
	Name           string
	Phone          string
	Email          string
	Location       string
	Source         string
	Portal         string
	Qualification  string
	SalesStatus    string
	Added          string
	Deal           string
	Team           string
	SalesExecutive string
	Analyst        string
	Tag            string
}

type LeadExportResult struct {
	Rows       []LeadExportRow
	MatchTotal int64
	Written    int
	Filter     string
	Sort       string
	Query      string
}

func (s *LeadStore) ExportLeadsPDF(ctx context.Context, params LeadListParams, meta leadExportMeta) ([]byte, LeadExportResult, error) {
	params.Filter = normalizeLeadFilter(params.Filter)
	params.Sort = normalizeLeadSort(params.Sort)
	params.Query = strings.TrimSpace(params.Query)
	params.Field = canonicalSearchField(params.Field)

	where, args := buildLeadListWhere(params, 0)
	whereSQL := ""
	if len(where) > 0 {
		whereSQL = "WHERE " + strings.Join(where, " AND ")
	}

	tx, err := s.pool.BeginTx(ctx, pgx.TxOptions{})
	if err != nil {
		return nil, LeadExportResult{}, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	if _, err := tx.Exec(ctx, `SET LOCAL statement_timeout = '180s'`); err != nil {
		return nil, LeadExportResult{}, err
	}

	var matchTotal int64
	countSQL := `SELECT COUNT(*)::bigint FROM "Lead" l ` + whereSQL
	if err := tx.QueryRow(ctx, countSQL, args...).Scan(&matchTotal); err != nil {
		return nil, LeadExportResult{}, err
	}

	doc := newLeadExportDoc(meta, matchTotal)

	listSQL := fmt.Sprintf(`
		SELECT
			l."leadName",
			l.phone,
			l."leadEmail",
			l.country,
			l.city,
			l.source,
			l."portalWebsite",
			l."qualificationStatus",
			l."salesStage",
			l."estimatedDealValue",
			l."closedRevenue",
			l."dealCurrency",
			l."createdAt",
			l."notAppropriate",
			COALESCE(cb.name, '—'),
			COALESCE(t.name, '—'),
			COALESCE(se.name, '—')
		FROM "Lead" l
		LEFT JOIN "User" cb ON cb.id = l."createdById"
		LEFT JOIN "Team" t ON t.id = l."teamId"
		LEFT JOIN "User" se ON se.id = l."assignedSalesExecId"
		%s
		%s`, whereSQL, exportOrderSQL(params.Sort))

	rows, err := tx.Query(ctx, listSQL, args...)
	if err != nil {
		return nil, LeadExportResult{}, err
	}
	defer rows.Close()

	for rows.Next() {
		row, scanErr := scanLeadExportRow(rows)
		if scanErr != nil {
			return nil, LeadExportResult{}, scanErr
		}
		doc.Add(row)
	}
	if err := rows.Err(); err != nil {
		return nil, LeadExportResult{}, err
	}
	if err := tx.Commit(ctx); err != nil {
		return nil, LeadExportResult{}, err
	}

	pdf, err := doc.Bytes()
	if err != nil {
		return nil, LeadExportResult{}, err
	}
	return pdf, LeadExportResult{
		MatchTotal: matchTotal,
		Written:    doc.written,
		Filter:     params.Filter,
		Sort:       params.Sort,
		Query:      params.Query,
	}, nil
}

type exportRowScanner interface {
	Scan(dest ...any) error
}

func scanLeadExportRow(rows exportRowScanner) (LeadExportRow, error) {
	var (
		name, source, status, stage string
		phone, email, country, city *string
		portal                      *string
		deal, closedRevenue         *float64
		currency                    string
		createdAt                   time.Time
		notAppropriate              bool
		analyst, team, exec         string
	)
	if err := rows.Scan(
		&name, &phone, &email, &country, &city, &source, &portal,
		&status, &stage, &deal, &closedRevenue, &currency, &createdAt,
		&notAppropriate, &analyst, &team, &exec,
	); err != nil {
		return LeadExportRow{}, err
	}
	tag := "—"
	if notAppropriate {
		tag = "Not appropriate"
	}
	return LeadExportRow{
		Name:          emptyDash(name),
		Phone:         displayOrDash(phone),
		Email:         displayOrDash(email),
		Location:      formatLocation(city, country),
		Source:        emptyDash(source),
		Portal:        displayOrDash(portal),
		Qualification: qualificationDisplay(status),
		SalesStatus:   salesStageDisplay(stage),
		Added:         formatLeadCreatedAt(createdAt),
		Deal: formatDealValue(func() *float64 {
			if closedRevenue != nil {
				return closedRevenue
			}
			return deal
		}(), currency),
		Team:           emptyDash(team),
		SalesExecutive: emptyDash(exec),
		Analyst:        emptyDash(analyst),
		Tag:            tag,
	}, nil
}

func exportOrderSQL(sort string) string {
	switch sort {
	case "name":
		return `ORDER BY LOWER(l."leadName") ASC, l.id ASC`
	case "oldest":
		return `ORDER BY l."createdAt" ASC, l.id ASC`
	case "recent":
		return `ORDER BY l."updatedAt" DESC, l.id DESC`
	case "status":
		return `ORDER BY l."qualificationStatus" ASC, l."createdAt" DESC, l.id DESC`
	case "stage":
		return `ORDER BY l."salesStage" ASC, l."createdAt" DESC, l.id DESC`
	case "value":
		return `ORDER BY l."estimatedDealValue" DESC NULLS LAST, l."createdAt" DESC, l.id DESC`
	case "analyst":
		return `ORDER BY COALESCE(cb.name, '') ASC, l."createdAt" DESC, l.id DESC`
	default:
		return `ORDER BY l."createdAt" DESC, l.id DESC`
	}
}

func emptyDash(s string) string {
	if strings.TrimSpace(s) == "" || s == "—" {
		return "—"
	}
	return strings.TrimSpace(s)
}

type leadExportMeta struct {
	ExportedBy string
	RoleLabel  string
	ScopeNote  string
	Filters    []string
	Generated  time.Time
}

func describeLeadExportFilters(params LeadListParams) []string {
	var chips []string
	filter := normalizeLeadFilter(params.Filter)
	if filter != "" && filter != "all" {
		chips = append(chips, exportFilterLabel(filter))
	}
	if q := strings.TrimSpace(params.Query); q != "" {
		if field := strings.TrimSpace(params.Field); field != "" {
			chips = append(chips, fmt.Sprintf("Search %s: %s", field, q))
		} else {
			chips = append(chips, "Search: "+q)
		}
	}
	if v := strings.TrimSpace(params.Country); v != "" {
		chips = append(chips, "Country: "+v)
	}
	if v := strings.TrimSpace(params.City); v != "" {
		chips = append(chips, "City: "+v)
	}
	if v := strings.TrimSpace(params.Status); v != "" {
		chips = append(chips, "Qualification: "+qualificationDisplay(v))
	}
	if v := strings.TrimSpace(params.Stage); v != "" {
		chips = append(chips, "Sales status: "+salesStageDisplay(v))
	}
	if v := strings.TrimSpace(params.Source); v != "" {
		chips = append(chips, "Source: "+v)
	}
	if v := strings.TrimSpace(params.Portal); v != "" {
		chips = append(chips, "Portal: "+v)
	}
	if v := strings.TrimSpace(params.MetaProfile); v != "" {
		chips = append(chips, "Meta: "+v)
	}
	if v := strings.TrimSpace(params.ServiceLine); v != "" {
		chips = append(chips, "Service: "+v)
	}
	if v := strings.TrimSpace(params.QualificationReason); v != "" {
		chips = append(chips, "Reason: "+v)
	}
	if v := strings.TrimSpace(params.TeamID); v != "" {
		if v == "none" {
			chips = append(chips, "Team: Unassigned")
		} else {
			chips = append(chips, "Team filter applied")
		}
	}
	if v := strings.TrimSpace(params.AnalystID); v != "" {
		if v == "none" {
			chips = append(chips, "Analyst: Unassigned")
		} else {
			chips = append(chips, "Analyst filter applied")
		}
	}
	if v := strings.TrimSpace(params.SalesExecID); v != "" {
		if v == "none" {
			chips = append(chips, "Sales exec: Unassigned")
		} else {
			chips = append(chips, "Sales exec filter applied")
		}
	}
	from := strings.TrimSpace(params.AddedFrom)
	to := strings.TrimSpace(params.AddedTo)
	switch {
	case from != "" && to != "":
		chips = append(chips, "Added: "+from+" → "+to)
	case from != "":
		chips = append(chips, "Added from: "+from)
	case to != "":
		chips = append(chips, "Added to: "+to)
	}
	if len(chips) == 0 {
		return []string{"All leads in your current access scope"}
	}
	return chips
}

func exportFilterLabel(filter string) string {
	switch filter {
	case "new", "not_qualified":
		return "Not qualified"
	case "qualified":
		return "Qualified"
	case "irrelevant":
		return "Irrelevant"
	case "not_appropriate":
		return "Not appropriate"
	case "open":
		return "Open"
	case "contacted", "in_progress":
		return "In progress"
	case "converted":
		return "Closed"
	case "lost":
		return "Lost"
	case "passed", "assigned", "passed_se_tl":
		return "Passed to SE/TLs"
	case "not_passed":
		return "Not passed"
	case "with_team_lead":
		return "With team lead"
	case "with_sales_exec":
		return "With executive"
	default:
		return humanizeEnum(filter)
	}
}

func exportScopeNote(role string) string {
	switch {
	case isSalesExecutive(role):
		return "Sales Executive scope — assigned leads only"
	case isLeadAnalyst(role):
		return "Lead Analyst scope — leads you created"
	case isMainTeamLead(role):
		return "Main Team Lead scope — your team only"
	case isAnalystTeamLead(role):
		return "Analyst Team Lead scope — leads added by your Lead Analysts"
	default:
		return "Workspace scope"
	}
}

type leadExportDoc struct {
	pdf        *fpdf.Fpdf
	tr         func(string) string
	meta       leadExportMeta
	matchTotal int64
	written    int
	cols       []exportCol
}

func newLeadExportDoc(meta leadExportMeta, matchTotal int64) *leadExportDoc {
	pdf := fpdf.New("L", "mm", "A4", "")
	pdf.SetMargins(10, 10, 10)
	pdf.SetAutoPageBreak(false, 0)
	pdf.AliasNbPages("{nb}")
	tr := pdf.UnicodeTranslatorFromDescriptor("")
	pdf.SetFooterFunc(func() {
		pdf.SetY(-8)
		pdf.SetFont("Helvetica", "", 7)
		pdf.SetTextColor(108, 117, 125)
		pdf.CellFormat(0, 6, tr(fmt.Sprintf("LeadFlow confidential  ·  Page %d of {nb}", pdf.PageNo())), "", 0, "C", false, 0, "")
	})

	doc := &leadExportDoc{
		pdf:        pdf,
		tr:         tr,
		meta:       meta,
		matchTotal: matchTotal,
		cols:       exportTableCols(),
	}
	pdf.AddPage()
	doc.writeTitle(true)
	doc.writeTableHeader()
	if matchTotal == 0 {
		pdf.SetTextColor(108, 117, 125)
		pdf.SetFont("Helvetica", "", 10)
		pdf.Ln(8)
		pdf.CellFormat(0, 8, tr("No leads match the current filters."), "", 1, "C", false, 0, "")
	}
	return doc
}

func (d *leadExportDoc) Add(row LeadExportRow) {
	const rowH = 6.6
	const pageBottom = 198.0
	if d.pdf.GetY()+rowH > pageBottom {
		d.pdf.AddPage()
		d.writeTitle(false)
		d.writeTableHeader()
	}
	if d.written%2 == 0 {
		d.pdf.SetFillColor(248, 249, 250)
	} else {
		d.pdf.SetFillColor(255, 255, 255)
	}
	d.pdf.SetTextColor(33, 37, 41)
	d.pdf.SetFont("Helvetica", "", 6.5)
	values := []string{
		fmt.Sprintf("%d", d.written+1),
		row.Name,
		row.Phone,
		row.Email,
		row.Location,
		row.Source,
		row.Portal,
		row.Qualification,
		row.SalesStatus,
		row.Added,
		row.Deal,
	}
	for ci, col := range d.cols {
		align := "L"
		if ci == 0 {
			align = "R"
		}
		d.pdf.CellFormat(col.width, rowH, d.tr(clipPDF(values[ci], 42)), "0", 0, align, true, 0, "")
	}
	d.pdf.Ln(rowH)
	d.written++
}

func (d *leadExportDoc) writeTitle(full bool) {
	pdf, tr := d.pdf, d.tr
	pdf.SetFillColor(33, 37, 41)
	pdf.SetTextColor(255, 255, 255)
	pdf.SetFont("Helvetica", "B", 13)
	pdf.CellFormat(70, 9, tr("LeadFlow"), "0", 0, "L", true, 0, "")
	pdf.SetFont("Helvetica", "", 11)
	pdf.CellFormat(0, 9, tr("Leads export"), "0", 1, "R", true, 0, "")

	pdf.SetFillColor(232, 104, 18)
	pdf.CellFormat(0, 1.2, "", "0", 1, "", true, 0, "")
	pdf.Ln(3)

	if !full {
		pdf.SetTextColor(73, 80, 87)
		pdf.SetFont("Helvetica", "", 8)
		pdf.CellFormat(0, 5, tr(fmt.Sprintf("%s  ·  %s leads", d.meta.ScopeNote, formatInt64(d.matchTotal))), "", 1, "L", false, 0, "")
		pdf.Ln(1)
		return
	}

	pdf.SetTextColor(33, 37, 41)
	pdf.SetFont("Helvetica", "B", 9)
	when := d.meta.Generated.In(businessLocation()).Format("02 Jan 2006, 15:04 MST")
	pdf.CellFormat(0, 5, tr(fmt.Sprintf("Exported by %s (%s)  ·  %s", d.meta.ExportedBy, d.meta.RoleLabel, when)), "", 1, "L", false, 0, "")

	pdf.SetFont("Helvetica", "", 8)
	pdf.SetTextColor(73, 80, 87)
	pdf.CellFormat(0, 4.5, tr(d.meta.ScopeNote), "", 1, "L", false, 0, "")
	pdf.CellFormat(0, 4.5, tr("Filters: "+strings.Join(d.meta.Filters, "  ·  ")), "", 1, "L", false, 0, "")

	pdf.SetFont("Helvetica", "B", 8)
	pdf.SetTextColor(33, 37, 41)
	if d.matchTotal == 0 {
		pdf.CellFormat(0, 5, tr("0 matching leads"), "", 1, "L", false, 0, "")
	} else {
		pdf.CellFormat(0, 5, tr(fmt.Sprintf("%s matching leads", formatInt64(d.matchTotal))), "", 1, "L", false, 0, "")
	}
	pdf.Ln(2)
}

func (d *leadExportDoc) writeTableHeader() {
	d.pdf.SetFillColor(33, 37, 41)
	d.pdf.SetTextColor(255, 255, 255)
	d.pdf.SetFont("Helvetica", "B", 7)
	for _, col := range d.cols {
		d.pdf.CellFormat(col.width, 7, d.tr(col.label), "0", 0, "L", true, 0, "")
	}
	d.pdf.Ln(7)
}

func (d *leadExportDoc) Bytes() ([]byte, error) {
	var buf bytes.Buffer
	if err := d.pdf.Output(&buf); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

func buildLeadsExportPDF(result LeadExportResult, meta leadExportMeta) ([]byte, error) {
	total := result.MatchTotal
	if total == 0 {
		total = int64(len(result.Rows))
	}
	doc := newLeadExportDoc(meta, total)
	for _, row := range result.Rows {
		doc.Add(row)
	}
	return doc.Bytes()
}

type exportCol struct {
	label string
	width float64
}

func exportTableCols() []exportCol {
	return []exportCol{
		{"#", 8},
		{"Name", 32},
		{"Phone", 26},
		{"Email", 38},
		{"Location", 26},
		{"Source", 24},
		{"Portal", 30},
		{"Qualification", 24},
		{"Sales status", 26},
		{"Added", 16},
		{"Deal", 17},
	}
}

func clipPDF(s string, max int) string {
	s = strings.Join(strings.Fields(strings.TrimSpace(s)), " ")
	if s == "" {
		return "—"
	}
	runes := []rune(s)
	if len(runes) <= max {
		return s
	}
	return string(runes[:max-1]) + "…"
}

func formatInt64(n int64) string {
	s := fmt.Sprintf("%d", n)
	if n < 1000 && n > -1000 {
		return s
	}
	neg := ""
	if n < 0 {
		neg = "-"
		s = s[1:]
	}
	var b strings.Builder
	b.WriteString(neg)
	left := len(s) % 3
	if left == 0 {
		left = 3
	}
	b.WriteString(s[:left])
	for i := left; i < len(s); i += 3 {
		b.WriteByte(',')
		b.WriteString(s[i : i+3])
	}
	return b.String()
}
