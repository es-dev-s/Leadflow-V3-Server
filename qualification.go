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

func isAssignableQualification(status string) bool {
	switch strings.TrimSpace(status) {
	case QualQualified, QualQualifiedChat, QualQualifiedCall, QualPaid, QualOrganic:
		return true
	default:
		return false
	}
}

func isAllowedQualification(status string) bool {
	_, ok := allowedQualifications[strings.TrimSpace(status)]
	return ok
}

// qualificationDisplay matches lead_flow_ui QUALIFICATION_OPTIONS labels.
func qualificationDisplay(status string) string {
	if label, ok := qualificationLabels[status]; ok {
		return label
	}
	return humanizeEnum(status)
}
