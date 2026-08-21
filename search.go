package main

import (
	"fmt"
	"strings"
)

func normalizeSearchField(field string) string {
	switch strings.ToLower(strings.TrimSpace(field)) {
	case "", "all", "global":
		return ""
	case "source", "portal", "lead", "analyst", "tag", "phone", "email",
		"clientprofile", "client_profile", "location", "analystnotes", "analyst_notes",
		"status", "score", "stage", "closed", "closeddate", "closed_date", "ip", "executivenotes", "executive_notes",
		"added", "team", "handoff", "contact", "duplicatecheck", "duplicate_check",
		"dealvalue", "deal_value", "salesexecutive", "sales_executive":
		return strings.ToLower(strings.ReplaceAll(strings.TrimSpace(field), "-", ""))
	default:
		return ""
	}
}

func canonicalSearchField(field string) string {
	switch normalizeSearchField(field) {
	case "client_profile", "clientprofile":
		return "clientProfile"
	case "analyst_notes", "analystnotes":
		return "analystNotes"
	case "executive_notes", "executivenotes":
		return "executiveNotes"
	case "deal_value", "dealvalue":
		return "dealValue"
	case "sales_executive", "salesexecutive":
		return "salesExecutive"
	case "duplicate_check", "duplicatecheck":
		return "duplicateCheck"
	case "closed_date", "closeddate":
		return "closedDate"
	case "source", "portal", "lead", "analyst", "tag", "phone", "email",
		"location", "status", "score", "stage", "closed", "ip", "added",
		"team", "handoff", "contact":
		return normalizeSearchField(field)
	default:
		return ""
	}
}

func appendPhoneSearch(where *[]string, args *[]any, q string) {
	pattern := "%" + escapeILIKE(q) + "%"
	*args = append(*args, pattern)
	n := len(*args)
	digits := digitsOnly(q)
	if len(digits) >= 2 {
		*args = append(*args, "%"+digits+"%")
		dn := len(*args)
		*where = append(*where, fmt.Sprintf(`(
			COALESCE(l.phone, '') ILIKE $%d ESCAPE '\'
			OR translate(COALESCE(l.phone, ''), ' ()-+.', '') LIKE $%d
		)`, n, dn))
		return
	}
	*where = append(*where, fmt.Sprintf(`COALESCE(l.phone, '') ILIKE $%d ESCAPE '\'`, n))
}

func appendTextSearch(where *[]string, args *[]any, expr, q string) {
	*args = append(*args, "%"+escapeILIKE(q)+"%")
	n := len(*args)
	*where = append(*where, fmt.Sprintf(`(%s) ILIKE $%d ESCAPE '\'`, expr, n))
}

func appendLeadSearch(where *[]string, args *[]any, q, field string) {
	q = strings.TrimSpace(q)
	if len([]rune(q)) < 2 {
		return
	}
	field = canonicalSearchField(field)
	patternN := func() int {
		*args = append(*args, "%"+escapeILIKE(q)+"%")
		return len(*args)
	}

	switch field {
	case "source":
		appendTextSearch(where, args, `l.source`, q)
	case "portal":
		appendTextSearch(where, args, `COALESCE(l."portalWebsite", '')`, q)
	case "lead":
		appendTextSearch(where, args, `l."leadName"`, q)
	case "analyst":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM "User" a
			WHERE a.id = l."createdById"
			  AND (a.name ILIKE $%d ESCAPE '\' OR COALESCE(a.email, '') ILIKE $%d ESCAPE '\')
		)`, n, n))
	case "tag":
		lower := strings.ToLower(q)
		tagClauses := make([]string, 0, 3)
		if strings.Contains(lower, "new") {
			tagClauses = append(tagClauses, leadIsUnassignedSQL)
		}
		if strings.Contains(lower, "appropriate") || strings.Contains(lower, "approved") {
			tagClauses = append(tagClauses, `l."notAppropriate" = TRUE`)
		}
		if strings.Contains(lower, "irrelevant") {
			tagClauses = append(tagClauses, `l."qualificationStatus" = 'IRRELEVANT'`)
		}
		if len(tagClauses) == 0 {
			*where = append(*where, `FALSE`)
		} else {
			*where = append(*where, "("+strings.Join(tagClauses, " OR ")+")")
		}
	case "phone":
		appendPhoneSearch(where, args, q)
	case "email":
		appendTextSearch(where, args, `COALESCE(l."leadEmail", '')`, q)
	case "clientProfile":
		appendTextSearch(where, args, `COALESCE(l."clientProfile", '')`, q)
	case "location":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`(
			COALESCE(l.city, '') ILIKE $%d ESCAPE '\'
			OR COALESCE(l.country, '') ILIKE $%d ESCAPE '\'
			OR (COALESCE(l.city, '') || ', ' || COALESCE(l.country, '')) ILIKE $%d ESCAPE '\'
		)`, n, n, n))
	case "analystNotes":
		appendTextSearch(where, args, `COALESCE(l.notes, '')`, q)
	case "status":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`(
			l."qualificationStatus" ILIKE $%d ESCAPE '\'
			OR replace(lower(l."qualificationStatus"), '_', ' ') ILIKE $%d ESCAPE '\'
		)`, n, n))
	case "score":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`COALESCE(l."leadScore"::text, '') ILIKE $%d ESCAPE '\'`, n))
	case "stage":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`(
			l."salesStage" ILIKE $%d ESCAPE '\'
			OR replace(lower(l."salesStage"), '_', ' ') ILIKE $%d ESCAPE '\'
		)`, n, n))
	case "closed":
		lower := strings.ToLower(q)
		if strings.Contains(lower, "lost") {
			*where = append(*where, `l."salesStage" = 'CLOSED_LOST'`)
		} else if strings.Contains(lower, "open") {
			*where = append(*where, `(
				l."closedAt" IS NULL
				AND l."salesStage" NOT IN ('CLOSED_WON', 'CLOSED_LOST')
			)`)
		} else if strings.Contains(lower, "clos") {
			*where = append(*where, `(
				l."closedAt" IS NOT NULL
				OR l."salesStage" IN ('CLOSED_WON', 'CLOSED_LOST')
			)`)
		} else {
			*where = append(*where, `FALSE`)
		}
	case "closedDate":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`(
			l."closedAt" IS NOT NULL
			AND (
				to_char(l."closedAt", 'FMMM/FMDD/YYYY') ILIKE $%d ESCAPE '\'
				OR to_char(l."closedAt", 'YYYY-MM-DD') ILIKE $%d ESCAPE '\'
				OR to_char(l."closedAt", 'FMMonth FMDD, YYYY') ILIKE $%d ESCAPE '\'
			)
		)`, n, n, n))
	case "ip", "duplicateCheck":
		// No persisted values in current schema.
		*where = append(*where, `FALSE`)
	case "executiveNotes":
		appendTextSearch(where, args, `COALESCE(l."lostNotes", '')`, q)
	case "added":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`(
			to_char(l."createdAt", 'FMMM/FMDD/YYYY') ILIKE $%d ESCAPE '\'
			OR to_char(l."createdAt", 'YYYY-MM-DD') ILIKE $%d ESCAPE '\'
			OR to_char(l."createdAt", 'FMMonth FMDD, YYYY') ILIKE $%d ESCAPE '\'
		)`, n, n, n))
	case "team":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`(
			EXISTS (
				SELECT 1 FROM "Team" t
				WHERE t.id = l."teamId" AND t.name ILIKE $%d ESCAPE '\'
			)
			OR EXISTS (
				SELECT 1 FROM "User" mtl
				WHERE mtl.id = l."assignedMainTeamLeadId" AND mtl.name ILIKE $%d ESCAPE '\'
			)
		)`, n, n))
	case "handoff":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM "LeadHandoffLog" h
			WHERE h."leadId" = l.id
			  AND h.action <> 'LEAD_CREATED'
			  AND (
				h.action ILIKE $%d ESCAPE '\'
				OR replace(lower(h.action), '_', ' ') ILIKE $%d ESCAPE '\'
				OR COALESCE(h.detail, '') ILIKE $%d ESCAPE '\'
			  )
		)`, n, n, n))
	case "contact":
		n := patternN()
		digits := digitsOnly(q)
		if len(digits) >= 2 {
			*args = append(*args, "%"+digits+"%")
			dn := len(*args)
			*where = append(*where, fmt.Sprintf(`(
				COALESCE(l.phone, '') ILIKE $%d ESCAPE '\'
				OR translate(COALESCE(l.phone, ''), ' ()-+.', '') LIKE $%d
				OR COALESCE(l.city, '') ILIKE $%d ESCAPE '\'
				OR COALESCE(l.country, '') ILIKE $%d ESCAPE '\'
			)`, n, dn, n, n))
		} else {
			*where = append(*where, fmt.Sprintf(`(
				COALESCE(l.phone, '') ILIKE $%d ESCAPE '\'
				OR COALESCE(l.city, '') ILIKE $%d ESCAPE '\'
				OR COALESCE(l.country, '') ILIKE $%d ESCAPE '\'
			)`, n, n, n))
		}
	case "dealValue":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`(
			COALESCE(l."estimatedDealValue"::text, '') ILIKE $%d ESCAPE '\'
			OR COALESCE(l."dealCurrency", '') ILIKE $%d ESCAPE '\'
		)`, n, n))
	case "salesExecutive":
		n := patternN()
		*where = append(*where, fmt.Sprintf(`EXISTS (
			SELECT 1 FROM "User" se
			WHERE se.id = l."assignedSalesExecId" AND se.name ILIKE $%d ESCAPE '\'
		)`, n))
	default:
		// All-fields search runs against the precomputed lower-cased search
		// document ("searchDoc", gin trgm indexed). The previous 18-column OR
		// with EXISTS subqueries could not use any index and blew the 8s
		// statement timeout on 1M rows (HTTP 500 in the leads list).
		// Cross-entity searches (analyst, team, handoff…) remain available via
		// the field-specific search options.
		*args = append(*args, "%"+escapeILIKE(strings.ToLower(q))+"%")
		n := len(*args)
		clause := fmt.Sprintf(`l."searchDoc" LIKE $%d ESCAPE '\'`, n)
		if digits := digitsOnly(q); len(digits) >= 4 {
			// The doc embeds phone digits stripped of separators, so a
			// formatted phone query still matches.
			*args = append(*args, "%"+digits+"%")
			clause = fmt.Sprintf(`(%s OR l."searchDoc" LIKE $%d)`, clause, len(*args))
		}
		*where = append(*where, clause)
	}
}
