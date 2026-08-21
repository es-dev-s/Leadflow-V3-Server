package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode"
)

type exportFlightLock struct {
	mu     sync.Mutex
	active map[string]time.Time
}

var leadExportFlights = &exportFlightLock{active: map[string]time.Time{}}

func (l *exportFlightLock) acquire(userID string) bool {
	l.mu.Lock()
	defer l.mu.Unlock()
	if started, ok := l.active[userID]; ok && time.Since(started) < 10*time.Minute {
		return false
	}
	l.active[userID] = time.Now()
	return true
}

func (l *exportFlightLock) release(userID string) {
	l.mu.Lock()
	delete(l.active, userID)
	l.mu.Unlock()
}

func (s *Server) handleLeadsExport(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}

	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	if !leadExportFlights.acquire(authUser.ID) {
		writeError(w, http.StatusTooManyRequests, "an export is already running — wait for it to finish")
		return
	}
	defer leadExportFlights.release(authUser.ID)

	q := r.URL.Query()
	params := s.leadListParamsFromRequest(r)
	params.Sort = q.Get("sort")
	params.Query = q.Get("q")
	params.Field = q.Get("field")

	reqCtx, cancel := context.WithTimeout(r.Context(), 3*time.Minute)
	defer cancel()

	meta := leadExportMeta{
		ExportedBy: firstNonEmpty(authUser.Name, authUser.Email, "Unknown user"),
		RoleLabel:  roleLabel(authUser.Role),
		ScopeNote:  exportScopeNote(authUser.Role),
		Filters:    describeLeadExportFilters(params),
		Generated:  time.Now(),
	}

	pdf, result, err := s.leads.ExportLeadsPDF(reqCtx, params, meta)
	if err != nil {
		log.Printf("export leads: %v", err)
		if reqCtx.Err() != nil {
			writeError(w, http.StatusGatewayTimeout, "export timed out — try again")
			return
		}
		writeError(w, http.StatusInternalServerError, "failed to export leads")
		return
	}

	filename := leadExportFilename(params, meta.Generated)
	w.Header().Set("Content-Type", "application/pdf")
	w.Header().Set("Content-Disposition", fmt.Sprintf(`attachment; filename="%s"`, filename))
	w.Header().Set("Cache-Control", "no-store")
	w.Header().Set("X-LeadFlow-Export-Count", strconv.Itoa(result.Written))
	w.Header().Set("X-LeadFlow-Export-Total", strconv.FormatInt(result.MatchTotal, 10))
	w.Header().Set("X-LeadFlow-Export-Truncated", "false")
	w.WriteHeader(http.StatusOK)
	if _, err := w.Write(pdf); err != nil {
		log.Printf("export leads write: %v", err)
	}
}

func leadExportFilename(params LeadListParams, when time.Time) string {
	stamp := when.In(businessLocation()).Format("2006-01-02")
	filter := normalizeLeadFilter(params.Filter)
	name := "leadflow-leads-" + stamp
	if filter != "" && filter != "all" {
		name += "-" + sanitizeFilenamePart(filter)
	}
	if q := strings.TrimSpace(params.Query); q != "" {
		name += "-search"
	}
	if strings.TrimSpace(params.Country) != "" || strings.TrimSpace(params.Status) != "" ||
		strings.TrimSpace(params.Stage) != "" || strings.TrimSpace(params.Source) != "" ||
		strings.TrimSpace(params.Portal) != "" || strings.TrimSpace(params.AddedFrom) != "" ||
		strings.TrimSpace(params.AddedTo) != "" {
		name += "-filtered"
	}
	return name + ".pdf"
}

func sanitizeFilenamePart(s string) string {
	s = strings.ToLower(strings.TrimSpace(s))
	var b strings.Builder
	for _, r := range s {
		switch {
		case unicode.IsLetter(r) || unicode.IsDigit(r):
			b.WriteRune(r)
		case r == '-' || r == '_':
			b.WriteByte('-')
		default:
			b.WriteByte('-')
		}
	}
	out := strings.Trim(b.String(), "-")
	if out == "" {
		return "custom"
	}
	if len(out) > 32 {
		return out[:32]
	}
	return out
}
