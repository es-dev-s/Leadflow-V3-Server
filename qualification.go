package main

import "strings"

// Canonical lead qualification codes. Paid and Organic are assignable
// (same pipeline as Qualified / Qualified-Chat / Qualified-Call).
const (
	QualQualified     = "QUALIFIED"
	QualQualifiedChat = "QUALIFIED_CHAT"
	QualQualifiedCall = "QUALIFIED_CALL"
	QualPaid          = "PAID"
	QualOrganic       = "ORGANIC"
	QualNotQualified  = "NOT_QUALIFIED"
	QualIrrelevant    = "IRRELEVANT"
)

var qualificationLabels = map[string]string{
	QualQualified:     "Qualified",
	QualQualifiedChat: "Qualified - Chat",
	QualQualifiedCall: "Qualified - Call",
	QualPaid:          "Paid",
	QualOrganic:       "Organic",
	QualNotQualified:  "Not Qualified",
	QualIrrelevant:    "Irrelevant",
}

var assignableQualificationCodes = []string{
	QualQualified,
	QualQualifiedChat,
	QualQualifiedCall,
	QualPaid,
	QualOrganic,
}

var allowedQualifications = map[string]struct{}{
	QualQualified:     {},
	QualQualifiedChat: {},
	QualQualifiedCall: {},
	QualPaid:          {},
	QualOrganic:       {},
	QualNotQualified:  {},
	QualIrrelevant:    {},
}

// sqlAssignableQualificationIN is the SQL list for assignable / "qualified"
// statuses. Built from assignableQualificationCodes so filters, KPIs, and
// assignment cannot drift.
var sqlAssignableQualificationIN = func() string {
	parts := make([]string, len(assignableQualificationCodes))
	for i, code := range assignableQualificationCodes {
		parts[i] = "'" + code + "'"
	}
	return strings.Join(parts, ", ")
}()

func sqlInAssignableQualification(column string) string {
	return column + " IN (" + sqlAssignableQualificationIN + ")"
}

func sqlInAssignableQualificationFold(column string) string {
	return "UPPER(BTRIM(" + column + ")) IN (" + sqlAssignableQualificationIN + ")"
}

func normalizeQualification(status string) string {
	s := strings.TrimSpace(status)
	if s == "" {
		return ""
	}
	if _, ok := allowedQualifications[s]; ok {
		return s
	}
	key := strings.ToUpper(s)
	key = strings.ReplaceAll(key, "-", "_")
	key = strings.ReplaceAll(key, " ", "_")
	for strings.Contains(key, "__") {
		key = strings.ReplaceAll(key, "__", "_")
	}
	key = strings.Trim(key, "_")
	if _, ok := allowedQualifications[key]; ok {
		return key
	}
	for code, label := range qualificationLabels {
		if strings.EqualFold(strings.TrimSpace(label), s) {
			return code
		}
	}
	return s
}

func canonicalizeQualification(status string) (string, bool) {
	code := normalizeQualification(status)
	_, ok := allowedQualifications[code]
	return code, ok
}

func isAssignableQualification(status string) bool {
	code := normalizeQualification(status)
	switch code {
	case QualQualified, QualQualifiedChat, QualQualifiedCall, QualPaid, QualOrganic:
		return true
	default:
		return false
	}
}

func isAllowedQualification(status string) bool {
	_, ok := canonicalizeQualification(status)
	return ok
}

// qualificationDisplay matches lead_flow_ui QUALIFICATION_OPTIONS labels.
func qualificationDisplay(status string) string {
	code := normalizeQualification(status)
	if label, ok := qualificationLabels[code]; ok {
		return label
	}
	return humanizeEnum(status)
}
