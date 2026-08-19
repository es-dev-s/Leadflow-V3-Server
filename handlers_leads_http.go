package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleLeads(w http.ResponseWriter, r *http.Request) {
	if !s.requireLeadDataAccess(w, r) {
		return
	}
	switch r.Method {
	case http.MethodGet:
		s.listLeads(w, r)
	case http.MethodPost:
		s.createLead(w, r)
	case http.MethodDelete:
		s.deleteLeads(w, r)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) listLeads(w http.ResponseWriter, r *http.Request) {
	// Viewer-scoped cache (key includes path+query+userID): repeat searches,
	// filter pages, and back-navigation return instantly. Any lead mutation
	// bumps the cache generation, so entries never outlive a data change.
	if s.serveFromCache(w, r) {
		return
	}
	q := r.URL.Query()
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	analystID := q.Get("analystId")
	teamID := q.Get("teamId")
	salesExecID := q.Get("salesExecId")
	if authUser, ok := userFromContext(r.Context()); ok {
		if owner := leadDataOwnerID(authUser.Role, authUser.ID); owner != "" {
			// Lead analysts may only list leads they created.
			analystID = owner
		}
		if seID := leadSalesExecScopeID(authUser.Role, authUser.ID); seID != "" {
			salesExecID = seID
		}
		if scopedTeam := leadTeamScopeID(authUser.Role, authUser.TeamID); scopedTeam != "" {
			teamID = scopedTeam
		} else if isMainTeamLead(authUser.Role) {
			teamID = "00000000-0000-0000-0000-000000000000"
		}
	}

	result, err := s.leads.List(reqCtx, LeadListParams{
		Filter:              q.Get("filter"),
		Sort:                q.Get("sort"),
		Query:               q.Get("q"),
		Field:               q.Get("field"),
		Cursor:              q.Get("cursor"),
		Limit:               limit,
		Country:             q.Get("country"),
		City:                q.Get("city"),
		TeamID:              teamID,
		AnalystID:           analystID,
		SalesExecID:         salesExecID,
		Source:              q.Get("source"),
		Portal:              q.Get("portal"),
		MetaProfile:         q.Get("metaProfile"),
		Status:              q.Get("status"),
		Stage:               q.Get("stage"),
		ServiceLine:         q.Get("serviceLine"),
		QualificationReason: firstNonEmpty(q.Get("reason"), q.Get("qualificationReason")),
		AddedFrom:           q.Get("addedFrom"),
		AddedTo:             q.Get("addedTo"),
	})
	if err != nil {
		if strings.Contains(err.Error(), "invalid cursor") {
			writeError(w, http.StatusBadRequest, "invalid cursor")
			return
		}
		log.Printf("list leads: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list leads")
		return
	}
	s.writeCachedJSON(w, r, result, 15*time.Second)
}

type CreateLeadRequest struct {
	FullName               string  `json:"fullName"`
	Email                  *string `json:"email"`
	Phone                  *string `json:"phone"`
	Country                *string `json:"country"`
	City                   *string `json:"city"`
	PortalWebsite          *string `json:"portalWebsite"`
	Source                 string  `json:"source"`
	SourceMetaProfileName  *string `json:"facebookProfile"`
	Language               *string `json:"language"`
	ClientProfile          *string `json:"clientProfile"`
	QualificationStatus    string  `json:"qualificationStatus"`
	LeadScore              *int    `json:"leadScore"`
	CreatedAt              *string `json:"createdAt"`
	Notes                  *string `json:"notes"`
	FirstClientMessageAt   *string `json:"firstClientMessageAt"`
	FirstAgentMessageAt    *string `json:"firstAgentMessageAt"`
	FirstResponseProofPath *string `json:"firstResponseProofPath"`
}

func normalizeFirstResponseProofPath(raw *string) (*string, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		empty := ""
		return &empty, nil
	}
	name := proofStoredNameFromPath(trimmed)
	if name == "" || strings.ContainsAny(name, `/\`) {
		return nil, fmt.Errorf("invalid first response proof path")
	}
	// Accept only our uploaded public path shape (or bare stored filename).
	if strings.HasPrefix(trimmed, "/api/uploads/first-response/") || trimmed == name {
		out := proofPublicPath(name)
		return &out, nil
	}
	return nil, fmt.Errorf("invalid first response proof path")
}

func parseOptionalDateTime(raw *string, field string) (*time.Time, error) {
	if raw == nil {
		return nil, nil
	}
	trimmed := strings.TrimSpace(*raw)
	if trimmed == "" {
		return nil, nil
	}
	// Absolute timestamps (include zone).
	if parsed, err := time.Parse(time.RFC3339Nano, trimmed); err == nil {
		return &parsed, nil
	}
	if parsed, err := time.Parse(time.RFC3339, trimmed); err == nil {
		return &parsed, nil
	}
	// Naive wall-clock values are interpreted in the business timezone.
	loc := businessLocation()
	layouts := []string{
		"2006-01-02T15:04",
		"2006-01-02T15:04:05",
		"2006-01-02 15:04",
		"2006-01-02 15:04:05",
	}
	var lastErr error
	for _, layout := range layouts {
		parsed, err := time.ParseInLocation(layout, trimmed, loc)
		if err == nil {
			return &parsed, nil
		}
		lastErr = err
	}
	_ = lastErr
	return nil, fmt.Errorf("invalid %s datetime", field)
}

// resolveFirstResponseTimes validates client/agent message times and derives minutes.
// Both must be set together (or both omitted). Agent time cannot be before client time.
func resolveFirstResponseTimes(clientRaw, agentRaw *string) (clientAt, agentAt *time.Time, minutes *int, err error) {
	clientAt, err = parseOptionalDateTime(clientRaw, "first client message")
	if err != nil {
		return nil, nil, nil, err
	}
	agentAt, err = parseOptionalDateTime(agentRaw, "first agent message")
	if err != nil {
		return nil, nil, nil, err
	}
	if clientAt == nil && agentAt == nil {
		return nil, nil, nil, nil
	}
	if clientAt == nil || agentAt == nil {
		return nil, nil, nil, fmt.Errorf("both first client message time and first agent message time are required")
	}
	if agentAt.Before(*clientAt) {
		return nil, nil, nil, fmt.Errorf("first agent message time cannot be before first client message time")
	}
	diff := int(agentAt.Sub(*clientAt).Minutes())
	if diff < 0 {
		diff = 0
	}
	if diff > 7*24*60 {
		return nil, nil, nil, fmt.Errorf("first response duration cannot exceed 7 days")
	}
	return clientAt, agentAt, &diff, nil
}

func optionalTrimmed(v *string) *string {
	if v == nil {
		return nil
	}
	trimmed := strings.TrimSpace(*v)
	if trimmed == "" {
		return nil
	}
	return &trimmed
}

func teamLabelFromMatch(m *LeadContactMatch) string {
	if m == nil {
		return "Unassigned"
	}
	if m.TeamName != nil && strings.TrimSpace(*m.TeamName) != "" {
		return strings.TrimSpace(*m.TeamName)
	}
	return "Unassigned"
}

func (s *Server) handleLeadContactLookup(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canCreateLeads(authUser.Role) && !canEditLeadProfile(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to look up leads")
		return
	}

	q := r.URL.Query()
	phone := strings.TrimSpace(q.Get("phone"))
	excludeID := strings.TrimSpace(q.Get("excludeId"))

	// Phone-only presence (team + portals/sources already used with this number).
	// Informational only — create/update is never blocked by phone presence.
	presence, err := s.leads.FindPhonePresence(r.Context(), phone, excludeID)
	if err != nil {
		log.Printf("contact lookup phone: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to look up contact")
		return
	}
	if presence == nil {
		writeJSON(w, http.StatusOK, map[string]any{
			"exists": false,
		})
		return
	}

	teamLabel := "Unassigned"
	if presence.TeamName != nil && strings.TrimSpace(*presence.TeamName) != "" {
		teamLabel = strings.TrimSpace(*presence.TeamName)
	}
	portals := presence.Portals
	if portals == nil {
		portals = []string{}
	}
	sources := presence.Sources
	if sources == nil {
		sources = []string{}
	}

	writeJSON(w, http.StatusOK, map[string]any{
		"exists":          true,
		"id":              presence.ID,
		"leadName":        presence.LeadName,
		"teamName":        teamLabel,
		"matchedOn":       "phone",
		"existingPortals": portals,
		"existingSources": sources,
	})
}

func (s *Server) createLead(w http.ResponseWriter, r *http.Request) {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canCreateLeads(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to create leads")
		return
	}
	var req CreateLeadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	v := &ValidationError{}
	req.FullName = optionalBoundedString(v, "fullName", req.FullName, 200)
	req.Source = requireString(v, "source", req.Source, 2, 80)
	req.QualificationStatus = requireString(v, "qualificationStatus", req.QualificationStatus, 2, 40)
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}

	email := optionalTrimmed(req.Email)
	phone := optionalTrimmed(req.Phone)

	var createdAt *time.Time
	if req.CreatedAt != nil && strings.TrimSpace(*req.CreatedAt) != "" {
		raw := strings.TrimSpace(*req.CreatedAt)
		loc := businessLocation()
		// Date-only is the product format; accept a few legacy datetime shapes too.
		parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			parsed, err = time.ParseInLocation("2006-01-02T15:04", raw, loc)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid createdAt date")
			return
		}
		// Persist as calendar date at business-timezone midnight.
		day := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, loc)
		createdAt = &day
	}

	clientAt, agentAt, responseMinutes, err := resolveFirstResponseTimes(
		req.FirstClientMessageAt,
		req.FirstAgentMessageAt,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	proofPath, err := normalizeFirstResponseProofPath(req.FirstResponseProofPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	createdByID := authUser.ID

	id, err := s.leads.Create(r.Context(), CreateLeadInput{
		FullName:               req.FullName,
		Email:                  email,
		Phone:                  phone,
		Country:                optionalTrimmed(req.Country),
		City:                   optionalTrimmed(req.City),
		PortalWebsite:          optionalTrimmed(req.PortalWebsite),
		Source:                 req.Source,
		SourceMetaProfileName:  optionalTrimmed(req.SourceMetaProfileName),
		Language:               optionalTrimmed(req.Language),
		ClientProfile:          optionalTrimmed(req.ClientProfile),
		QualificationStatus:    req.QualificationStatus,
		LeadScore:              req.LeadScore,
		CreatedAt:              createdAt,
		Notes:                  optionalTrimmed(req.Notes),
		FirstClientMessageAt:   clientAt,
		FirstAgentMessageAt:    agentAt,
		FirstResponseMinutes:   responseMinutes,
		FirstResponseProofPath: proofPath,
		CreatedByID:            createdByID,
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "invalid source") ||
			strings.Contains(msg, "invalid qualification") ||
			strings.Contains(msg, "lead score") ||
			strings.Contains(msg, "first ") ||
			strings.Contains(msg, "full name") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		log.Printf("create lead: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create lead")
		return
	}

	actorName := "Someone"
	actorID := createdByID
	if authUser, ok := userFromContext(r.Context()); ok {
		actorID = authUser.ID
		if strings.TrimSpace(authUser.Name) != "" {
			actorName = authUser.Name
		}
	}
	leadID := id
	s.notifications.NotifyStaff(
		r.Context(),
		actorID,
		NotifLeadAdded,
		"Lead added",
		actorName+" · "+req.FullName,
		&leadID,
	)
	s.emitLead(EvtLeadCreated, leadID, actorID)

	writeJSON(w, http.StatusCreated, map[string]any{"id": id})
}

func (s *Server) handleLeadByID(w http.ResponseWriter, r *http.Request) {
	id := strings.TrimPrefix(r.URL.Path, "/api/leads/")
	id = strings.Trim(id, "/")
	if id == "" || strings.Contains(id, "/") {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	// Reserved collection paths are registered separately; reject collisions.
	switch id {
	case "assign", "summary", "geography", "geo-options", "contact-lookup", "pipeline", "added-series", "export":
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	if !s.requireLeadAccess(w, r, id) {
		return
	}

	switch r.Method {
	case http.MethodGet:
		detail, err := s.leads.GetByID(r.Context(), id)
		if err != nil {
			if strings.Contains(strings.ToLower(err.Error()), "no rows") {
				writeError(w, http.StatusNotFound, "lead not found")
				return
			}
			log.Printf("get lead: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to load lead")
			return
		}
		writeJSON(w, http.StatusOK, detail)
	case http.MethodPatch:
		authUser, ok := userFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		var peek map[string]json.RawMessage
		if err := json.Unmarshal(body, &peek); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		_, hasQual := peek["qualificationStatus"]
		_, hasFullName := peek["fullName"]
		_, hasStage := peek["salesStage"]
		_, hasPayment := peek["initialPayment"]
		_, hasRevenue := peek["closedRevenue"]
		_, hasExecNotes := peek["executiveNotes"]
		_, hasNotAppropriate := peek["notAppropriate"]
		_, hasNotAppropriateReason := peek["notAppropriateReason"]
		isNotAppropriatePatch := (hasNotAppropriate || hasNotAppropriateReason) && !hasFullName && !hasQual
		if isNotAppropriatePatch {
			s.patchLeadNotAppropriate(w, r, id, body)
			return
		}
		isSalesOutcomePatch := (hasStage || hasPayment || hasRevenue || hasExecNotes) && !hasFullName && !hasQual
		if isSalesOutcomePatch {
			s.patchLeadSalesOutcome(w, r, id, body)
			return
		}
		if hasQual && !hasFullName {
			s.patchLeadQualification(w, r, id, body)
			return
		}
		if !canEditLeadProfile(authUser.Role) {
			writeError(w, http.StatusForbidden, "you do not have permission to edit lead details")
			return
		}
		r.Body = io.NopCloser(bytes.NewReader(body))
		s.updateLead(w, r, id)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type QualificationPatchRequest struct {
	QualificationStatus string `json:"qualificationStatus"`
}

type SalesOutcomePatchRequest struct {
	SalesStage     *string  `json:"salesStage"`
	InitialPayment *float64 `json:"initialPayment"`
	ClosedRevenue  *float64 `json:"closedRevenue"`
	ExecutiveNotes *string  `json:"executiveNotes"`
}

func (s *Server) patchLeadSalesOutcome(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canUpdateSalesOutcome(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to update sales outcome")
		return
	}

	var peek map[string]json.RawMessage
	if err := json.Unmarshal(body, &peek); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	var req SalesOutcomePatchRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	in := SalesOutcomeInput{
		HasStage:   peek["salesStage"] != nil,
		HasPayment: peek["initialPayment"] != nil,
		HasRevenue: peek["closedRevenue"] != nil,
		HasNotes:   peek["executiveNotes"] != nil,
	}
	if in.HasStage {
		if req.SalesStage == nil || strings.TrimSpace(*req.SalesStage) == "" {
			writeError(w, http.StatusBadRequest, "salesStage is required")
			return
		}
		in.SalesStage = strings.TrimSpace(*req.SalesStage)
	}
	if in.HasPayment {
		in.InitialPayment = req.InitialPayment
	}
	if in.HasRevenue {
		in.ClosedRevenue = req.ClosedRevenue
	}
	if in.HasNotes {
		if req.ExecutiveNotes == nil {
			empty := ""
			in.ExecutiveNotes = &empty
		} else {
			trimmed := strings.TrimSpace(*req.ExecutiveNotes)
			in.ExecutiveNotes = &trimmed
		}
	}

	detail, err := s.leads.UpdateSalesOutcome(r.Context(), id, in)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "invalid sales outcome") ||
			strings.Contains(msg, "closed revenue is required") ||
			strings.Contains(msg, "cannot be negative") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, "lead not found")
			return
		}
		log.Printf("patch sales outcome: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update sales outcome")
		return
	}

	actorID, actorName := authUser.ID, "Someone"
	if strings.TrimSpace(authUser.Name) != "" {
		actorName = authUser.Name
	}
	leadName := detail.FullName
	if strings.TrimSpace(leadName) == "" {
		leadName = id
	}
	leadID := id
	s.notifications.NotifyStaff(
		r.Context(),
		actorID,
		NotifLeadUpdated,
		"Sales outcome updated",
		actorName+" · "+leadName+" · "+detail.SalesStageLabel,
		&leadID,
	)
	s.emitLead(EvtLeadUpdated, leadID, actorID)

	writeJSON(w, http.StatusOK, detail)
}

type NotAppropriatePatchRequest struct {
	NotAppropriate       *bool   `json:"notAppropriate"`
	NotAppropriateReason *string `json:"notAppropriateReason"`
}

func (s *Server) patchLeadNotAppropriate(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canMarkNotAppropriate(authUser.Role) {
		writeError(w, http.StatusForbidden, "only sales executives can mark leads as not appropriate")
		return
	}

	var req NotAppropriatePatchRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.NotAppropriate != nil && !*req.NotAppropriate {
		writeError(w, http.StatusBadRequest, "clearing not-appropriate flag is not supported")
		return
	}
	reason := ""
	if req.NotAppropriateReason != nil {
		reason = strings.TrimSpace(*req.NotAppropriateReason)
	}
	if reason == "" {
		writeError(w, http.StatusBadRequest, "notAppropriateReason is required")
		return
	}

	detail, err := s.leads.MarkNotAppropriate(r.Context(), id, reason, authUser.ID)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "reason is required") ||
			strings.Contains(msg, "reason must be") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		if strings.Contains(msg, "already marked") {
			writeError(w, http.StatusConflict, msg)
			return
		}
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, "lead not found")
			return
		}
		log.Printf("mark not appropriate: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to mark lead as not appropriate")
		return
	}

	actorID, actorName := authUser.ID, "Someone"
	if strings.TrimSpace(authUser.Name) != "" {
		actorName = authUser.Name
	}
	leadName := detail.FullName
	if strings.TrimSpace(leadName) == "" {
		leadName = id
	}
	leadID := id
	s.notifications.NotifyStaff(
		r.Context(),
		actorID,
		NotifLeadUpdated,
		"Lead marked not appropriate",
		actorName+" · "+leadName+" · Irrelevant",
		&leadID,
	)
	s.emitLead(EvtLeadUpdated, leadID, actorID)

	writeJSON(w, http.StatusOK, detail)
}

func (s *Server) patchLeadQualification(w http.ResponseWriter, r *http.Request, id string, body []byte) {
	if authUser, ok := userFromContext(r.Context()); ok && !canChangeQualification(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to change qualification status")
		return
	}
	var req QualificationPatchRequest
	dec := json.NewDecoder(bytes.NewReader(body))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	status := strings.TrimSpace(req.QualificationStatus)
	if status == "" {
		writeError(w, http.StatusBadRequest, "qualificationStatus is required")
		return
	}
	actorID := ""
	if authUser, ok := userFromContext(r.Context()); ok {
		actorID = authUser.ID
	}
	updated, err := s.leads.UpdateQualificationStatus(r.Context(), id, status, actorID)
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "invalid qualification") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, "lead not found")
			return
		}
		log.Printf("patch qualification: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update qualification")
		return
	}

	actorID, actorName := "", "Someone"
	if authUser, ok := userFromContext(r.Context()); ok {
		actorID = authUser.ID
		if strings.TrimSpace(authUser.Name) != "" {
			actorName = authUser.Name
		}
	}
	leadName := id
	if detail, err := s.leads.GetByID(r.Context(), id); err == nil && strings.TrimSpace(detail.FullName) != "" {
		leadName = detail.FullName
	}
	leadID := id
	s.notifications.NotifyStaff(
		r.Context(),
		actorID,
		NotifLeadUpdated,
		"Lead edited",
		actorName+" · "+leadName+" · "+qualificationDisplay(updated),
		&leadID,
	)
	s.emitLead(EvtLeadUpdated, leadID, actorID)

	writeJSON(w, http.StatusOK, map[string]any{
		"id":                  id,
		"qualificationStatus": updated,
		"status":              qualificationDisplay(updated),
		"updated":             true,
	})
}

func (s *Server) updateLead(w http.ResponseWriter, r *http.Request, id string) {
	var req CreateLeadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Main Team Leads (and others without qualify permission) may edit fields
	// but cannot change qualification — keep the stored value.
	if authUser, ok := userFromContext(r.Context()); ok && !canChangeQualification(authUser.Role) {
		detail, err := s.leads.GetByID(r.Context(), id)
		if err != nil {
			writeError(w, http.StatusNotFound, "lead not found")
			return
		}
		req.QualificationStatus = detail.QualificationStatus
	}

	v := &ValidationError{}
	req.FullName = optionalBoundedString(v, "fullName", req.FullName, 200)
	req.Source = requireString(v, "source", req.Source, 2, 80)
	req.QualificationStatus = requireString(v, "qualificationStatus", req.QualificationStatus, 2, 40)
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}

	email := optionalTrimmed(req.Email)
	phone := optionalTrimmed(req.Phone)

	var createdAt *time.Time
	if req.CreatedAt != nil && strings.TrimSpace(*req.CreatedAt) != "" {
		raw := strings.TrimSpace(*req.CreatedAt)
		loc := businessLocation()
		parsed, err := time.ParseInLocation("2006-01-02", raw, loc)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			parsed, err = time.ParseInLocation("2006-01-02T15:04", raw, loc)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid createdAt date")
			return
		}
		day := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, loc)
		createdAt = &day
	}

	clientAt, agentAt, responseMinutes, err := resolveFirstResponseTimes(
		req.FirstClientMessageAt,
		req.FirstAgentMessageAt,
	)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}
	proofPath, err := normalizeFirstResponseProofPath(req.FirstResponseProofPath)
	if err != nil {
		writeError(w, http.StatusBadRequest, err.Error())
		return
	}

	actorID := ""
	if authUser, ok := userFromContext(r.Context()); ok {
		actorID = authUser.ID
	}
	err = s.leads.Update(r.Context(), id, CreateLeadInput{
		FullName:               req.FullName,
		Email:                  email,
		Phone:                  phone,
		Country:                optionalTrimmed(req.Country),
		City:                   optionalTrimmed(req.City),
		PortalWebsite:          optionalTrimmed(req.PortalWebsite),
		Source:                 req.Source,
		SourceMetaProfileName:  optionalTrimmed(req.SourceMetaProfileName),
		Language:               optionalTrimmed(req.Language),
		ClientProfile:          optionalTrimmed(req.ClientProfile),
		QualificationStatus:    req.QualificationStatus,
		LeadScore:              req.LeadScore,
		CreatedAt:              createdAt,
		Notes:                  optionalTrimmed(req.Notes),
		FirstClientMessageAt:   clientAt,
		FirstAgentMessageAt:    agentAt,
		FirstResponseMinutes:   responseMinutes,
		FirstResponseProofPath: proofPath,
		CreatedByID:            actorID,
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "not found") {
			writeError(w, http.StatusNotFound, "lead not found")
			return
		}
		if strings.Contains(msg, "invalid source") ||
			strings.Contains(msg, "invalid qualification") ||
			strings.Contains(msg, "lead score") ||
			strings.Contains(msg, "first ") ||
			strings.Contains(msg, "full name") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		log.Printf("update lead: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update lead")
		return
	}

	actorID, actorName := "", "Someone"
	if authUser, ok := userFromContext(r.Context()); ok {
		actorID = authUser.ID
		if strings.TrimSpace(authUser.Name) != "" {
			actorName = authUser.Name
		}
	}
	leadID := id
	s.notifications.NotifyStaff(
		r.Context(),
		actorID,
		NotifLeadUpdated,
		"Lead edited",
		actorName+" · "+req.FullName,
		&leadID,
	)
	s.emitLead(EvtLeadUpdated, leadID, actorID)

	writeJSON(w, http.StatusOK, map[string]any{"id": id, "updated": true})
}

type BulkLeadIDsRequest struct {
	LeadIDs []string `json:"leadIds"`
}

type AssignLeadsRequest struct {
	LeadIDs    []string `json:"leadIds"`
	AssigneeID string   `json:"assigneeId"`
	Kind       string   `json:"kind"`
}

func (s *Server) deleteLeads(w http.ResponseWriter, r *http.Request) {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canDeleteLeads(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to delete leads")
		return
	}
	var req BulkLeadIDsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.LeadIDs) > 100 {
		writeError(w, http.StatusBadRequest, "too many leads selected (max 100)")
		return
	}

	actorID, actorName := authUser.ID, "Someone"
	ownerID := leadDataOwnerID(authUser.Role, authUser.ID)
	teamScope := leadTeamScopeID(authUser.Role, authUser.TeamID)
	seScope := leadSalesExecScopeID(authUser.Role, authUser.ID)
	if strings.TrimSpace(authUser.Name) != "" {
		actorName = authUser.Name
	}
	briefs, _ := s.leads.LeadNameBriefs(r.Context(), req.LeadIDs)

	reqCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	n, err := s.leads.DeleteLeads(reqCtx, req.LeadIDs, ownerID, teamScope, seScope)
	if err != nil {
		if strings.Contains(err.Error(), "no leads") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("delete leads: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to delete leads")
		return
	}

	if n > 0 {
		if len(briefs) == 1 {
			for _, name := range briefs {
				s.notifications.NotifyStaff(
					r.Context(),
					actorID,
					NotifLeadDeleted,
					"Lead deleted",
					actorName+" · "+name,
					nil,
				)
			}
		} else {
			s.notifications.NotifyStaff(
				r.Context(),
				actorID,
				NotifLeadDeleted,
				"Leads deleted",
				fmt.Sprintf("%s · %d leads", actorName, n),
				nil,
			)
		}
		for _, deletedID := range req.LeadIDs {
			s.emitLead(EvtLeadDeleted, strings.TrimSpace(deletedID), actorID)
		}
	}

	writeJSON(w, http.StatusOK, map[string]any{"deleted": n})
}

func (s *Server) handleAssignableUsers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canMutateLeads(authUser.Role) || isSalesExecutive(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to list assignable users")
		return
	}
	kind := strings.TrimSpace(r.URL.Query().Get("type"))
	if kind == "" {
		if canAssignToTeamLeads(authUser.Role) {
			kind = "team-leads"
		} else {
			kind = "members"
		}
	}
	if kind == "team-leads" && !canAssignToTeamLeads(authUser.Role) {
		writeError(w, http.StatusForbidden, "team leads can only assign to members on their team")
		return
	}
	teamScope := actorLeadTeamID(r)
	if teamScope == "" && isMainTeamLead(authUser.Role) {
		writeError(w, http.StatusBadRequest, "your account is not linked to a team")
		return
	}
	users, err := s.leads.ListAssignableUsers(r.Context(), kind, teamScope)
	if err != nil {
		if strings.Contains(err.Error(), "invalid kind") {
			writeError(w, http.StatusBadRequest, "type must be team-leads or members")
			return
		}
		log.Printf("list assignable users: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list assignable users")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"users": users, "type": kind})
}

func (s *Server) handleAssignLeads(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canMutateLeads(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to assign leads")
		return
	}
	if isSalesExecutive(authUser.Role) {
		writeError(w, http.StatusForbidden, "sales executives cannot reassign leads")
		return
	}
	var req AssignLeadsRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if len(req.LeadIDs) > 100 {
		writeError(w, http.StatusBadRequest, "too many leads selected (max 100)")
		return
	}
	kind := strings.TrimSpace(req.Kind)
	if kind == "team-lead" && !canAssignToTeamLeads(authUser.Role) {
		writeError(w, http.StatusForbidden, "team leads can only assign to members on their team")
		return
	}
	actorID := authUser.ID
	ownerID := leadDataOwnerID(authUser.Role, authUser.ID)
	teamScope := leadTeamScopeID(authUser.Role, authUser.TeamID)
	seScope := leadSalesExecScopeID(authUser.Role, authUser.ID)
	if teamScope == "" && isMainTeamLead(authUser.Role) {
		writeError(w, http.StatusBadRequest, "your account is not linked to a team")
		return
	}
	reqCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	result, err := s.leads.AssignLeads(reqCtx, AssignLeadInput{
		LeadIDs:     req.LeadIDs,
		AssigneeID:  strings.TrimSpace(req.AssigneeID),
		Kind:        strings.TrimSpace(req.Kind),
		ActorID:     actorID,
		CreatedByID: ownerID,
		TeamID:      teamScope,
		SalesExecID: seScope,
	})
	if err != nil {
		msg := err.Error()
		if strings.Contains(msg, "no leads") ||
			strings.Contains(msg, "assignee") ||
			strings.Contains(msg, "invalid assign") ||
			strings.Contains(msg, "not a team") ||
			strings.Contains(msg, "only qualified") ||
			strings.Contains(msg, "not found") ||
			strings.Contains(msg, "not on your team") ||
			strings.Contains(msg, "not linked to a team") {
			writeError(w, http.StatusBadRequest, msg)
			return
		}
		log.Printf("assign leads: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to assign leads")
		return
	}

	actorName := "Someone"
	if authUser, ok := userFromContext(r.Context()); ok {
		if strings.TrimSpace(authUser.Name) != "" {
			actorName = authUser.Name
		}
	}
	assigneeName := strings.TrimSpace(req.AssigneeID)
	if users, err := s.leads.ListAssignableUsers(r.Context(), "team-leads", teamScope); err == nil {
		for _, u := range users {
			if u.ID == strings.TrimSpace(req.AssigneeID) {
				assigneeName = u.Name
				break
			}
		}
	}
	if assigneeName == strings.TrimSpace(req.AssigneeID) {
		if users, err := s.leads.ListAssignableUsers(r.Context(), "members", teamScope); err == nil {
			for _, u := range users {
				if u.ID == strings.TrimSpace(req.AssigneeID) {
					assigneeName = u.Name
					break
				}
			}
		}
	}

	briefs, _ := s.leads.LeadNameBriefs(r.Context(), req.LeadIDs)
	for _, assignment := range result.Assignments {
		leadName := briefs[assignment.LeadID]
		if leadName == "" {
			leadName = "Lead"
		}
		leadID := assignment.LeadID
		if kind == "team-lead" {
			s.notifications.NotifyStaff(
				r.Context(),
				actorID,
				NotifLeadTransfer,
				"Lead transferred",
				fmt.Sprintf("%s · %s → %s", actorName, leadName, assignment.Team),
				&leadID,
				strings.TrimSpace(req.AssigneeID),
			)
		} else {
			s.notifications.NotifyStaff(
				r.Context(),
				actorID,
				NotifLeadAssigned,
				"Lead assigned",
				fmt.Sprintf("%s · %s → %s", actorName, leadName, assigneeName),
				&leadID,
				strings.TrimSpace(req.AssigneeID),
			)
		}
		s.emitLead(EvtLeadUpdated, leadID, actorID)
	}

	writeJSON(w, http.StatusOK, result)
}
