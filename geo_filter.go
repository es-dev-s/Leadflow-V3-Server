package main

import (
	"fmt"
	"strings"
)

// GeoFilter scopes lead aggregates by country, city, and optional creator/team/SE.
type GeoFilter struct {
	Country     string
	City        string
	CreatedByID string // when set, only leads created by this user
	TeamID      string // when set, only leads on this team
	SalesExecID string // when set, only leads assigned to this SE
}

func parseGeoFilter(country, city string) GeoFilter {
	return GeoFilter{
		Country: strings.TrimSpace(country),
		City:    strings.TrimSpace(city),
	}
}

func (f GeoFilter) Active() bool {
	return f.Country != "" || f.City != "" || f.CreatedByID != "" || f.TeamID != "" || f.SalesExecID != ""
}

// isBlankGeoLabel treats Unknown / none / unassigned as the blank geo bucket.
func isBlankGeoLabel(value string) bool {
	switch strings.ToLower(strings.TrimSpace(value)) {
	case "unknown", "none", "unassigned", "blank":
		return true
	default:
		return false
	}
}

func blankGeoSQL(alias, column string) string {
	return fmt.Sprintf(
		`(%s"%s" IS NULL OR BTRIM(%s"%s") = '' OR LOWER(BTRIM(%s"%s")) IN ('unknown', 'none', 'unassigned', 'blank'))`,
		alias, column, alias, column, alias, column,
	)
}

// normalizedCountrySQL buckets blank/sentinel country values to "Unknown"
// so geography lists match lead filters and one another.
func normalizedCountrySQL(column string) string {
	return fmt.Sprintf(`CASE
		WHEN %s IS NULL
		  OR BTRIM(%s) = ''
		  OR LOWER(BTRIM(%s)) IN ('unknown', 'none', 'unassigned', 'blank')
		THEN 'Unknown'
		ELSE BTRIM(%s)
	END`, column, column, column, column)
}

// leadClause returns SQL predicates for Lead columns and matching args.
// alias should be "" or "l." (including the dot).
// argStart is the last used placeholder index (0 means next is $1).
func (f GeoFilter) leadClause(alias string, argStart int) (clause string, args []any) {
	var parts []string
	n := argStart

	if f.Country != "" {
		if isBlankGeoLabel(f.Country) {
			parts = append(parts, blankGeoSQL(alias, "country"))
		} else {
			n++
			args = append(args, f.Country)
			countryCol := `"country"`
			if alias != "" {
				countryCol = alias + `"country"`
			}
			parts = append(parts, fmt.Sprintf(`(%s) = $%d`, normalizedCountrySQL(countryCol), n))
		}
	}

	if f.City != "" {
		if isBlankGeoLabel(f.City) {
			parts = append(parts, blankGeoSQL(alias, "city"))
		} else {
			n++
			args = append(args, f.City)
			parts = append(parts, fmt.Sprintf(`BTRIM(%s"city") = $%d`, alias, n))
		}
	}

	if owner := strings.TrimSpace(f.CreatedByID); owner != "" {
		n++
		args = append(args, owner)
		parts = append(parts, fmt.Sprintf(`%s"createdById" = $%d`, alias, n))
	}

	if teamID := strings.TrimSpace(f.TeamID); teamID != "" {
		if teamID == "none" {
			parts = append(parts, fmt.Sprintf(`%s"teamId" IS NULL`, alias))
		} else {
			n++
			args = append(args, teamID)
			parts = append(parts, fmt.Sprintf(`%s"teamId" = $%d`, alias, n))
		}
	}

	if seID := strings.TrimSpace(f.SalesExecID); seID != "" {
		if seID == "none" {
			parts = append(parts, fmt.Sprintf(`%s"assignedSalesExecId" IS NULL`, alias))
		} else {
			n++
			args = append(args, seID)
			parts = append(parts, fmt.Sprintf(`%s"assignedSalesExecId" = $%d`, alias, n))
		}
	}

	return strings.Join(parts, " AND "), args
}

func (f GeoFilter) whereSQL(alias string) (string, []any) {
	clause, args := f.leadClause(alias, 0)
	if clause == "" {
		return "", nil
	}
	return "WHERE " + clause, args
}

func (f GeoFilter) andSQL(alias string, argStart int) (string, []any) {
	clause, args := f.leadClause(alias, argStart)
	if clause == "" {
		return "", nil
	}
	return " AND " + clause, args
}
