package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
)

func (s *Server) handleLeadsSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}
	if s.serveFromCache(w, r) {
		return
	}

	params := s.leadListParamsFromRequest(r)
	owner := actorLeadOwnerID(r)
	hideActive := owner != "" || actorLeadSalesExecID(r) != ""

	reqCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	summary, err := s.leads.Summary(reqCtx, params)
	if err != nil {
		log.Printf("leads summary: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load leads summary")
		return
	}
	if !hideActive {
		summary.ActiveUsers = int64(s.hub.OnlineCount(params.TeamID))
	}
	s.writeCachedJSON(w, r, summary, 15*time.Second)
}

func (s *Server) handleKPI(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	if !canViewLeadData(authUser.Role) || isSalesExecutive(authUser.Role) {
		writeError(w, http.StatusForbidden, "KPI is not available for this role")
		return
	}
	if s.serveFromCache(w, r) {
		return
	}

	params := s.leadListParamsFromRequest(r)
	reqCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	hcTeam := strings.TrimSpace(params.TeamID)
	if hcTeam == "" || hcTeam == "none" {
		hcTeam = actorLeadTeamID(r)
	}
	teamHC, err := s.users.CountTeamHeadcount(reqCtx, hcTeam)
	if err != nil {
		log.Printf("kpi headcount: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load KPI")
		return
	}

	snapshot, err := s.leads.KPI(reqCtx, params, teamHC)
	if err != nil {
		log.Printf("kpi snapshot: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load KPI")
		return
	}
	s.writeCachedJSON(w, r, snapshot, 15*time.Second)
}

func (s *Server) handleKPITargets(w http.ResponseWriter, r *http.Request) {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}

	switch r.Method {
	case http.MethodGet:
		if !canViewLeadData(authUser.Role) || isSalesExecutive(authUser.Role) {
			writeError(w, http.StatusForbidden, "forbidden")
			return
		}
		reqCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		targets, err := s.leads.ListKpiTargets(reqCtx)
		if err != nil {
			log.Printf("kpi targets list: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to load KPI targets")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": targets})

	case http.MethodPut:
		if authUser.Role != RoleSuperadmin {
			writeError(w, http.StatusForbidden, "only Superadmin can edit KPI targets")
			return
		}
		var req struct {
			Items []KpiTargetUpdate `json:"items"`
		}
		if err := decodeJSON(r, &req); err != nil {
			writeError(w, http.StatusBadRequest, "invalid JSON body")
			return
		}
		if len(req.Items) == 0 {
			writeError(w, http.StatusBadRequest, "items are required")
			return
		}
		reqCtx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
		defer cancel()
		if err := s.leads.UpdateKpiTargets(reqCtx, req.Items); err != nil {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		if s.respCache != nil {
			s.respCache.Clear()
		}
		targets, err := s.leads.ListKpiTargets(reqCtx)
		if err != nil {
			writeJSON(w, http.StatusOK, map[string]any{"ok": true})
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"items": targets})

	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) handleLeadsPipelineSummary(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}
	if s.serveFromCache(w, r) {
		return
	}

	params := s.leadListParamsFromRequest(r)
	reqCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	summary, err := s.leads.PipelineSummary(reqCtx, params)
	if err != nil {
		log.Printf("leads pipeline summary: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load pipeline summary")
		return
	}
	s.writeCachedJSON(w, r, summary, 15*time.Second)
}

func (s *Server) handleLeadsSummaryBuckets(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}

	if s.serveFromCache(w, r) {
		return
	}

	q := r.URL.Query()
	dimension := strings.TrimSpace(q.Get("dimension"))
	if dimension == "" {
		writeError(w, http.StatusBadRequest, "dimension is required")
		return
	}

	offset := 0
	if raw := strings.TrimSpace(q.Get("offset")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			offset = n
		}
	}
	limit := 0
	if raw := strings.TrimSpace(q.Get("limit")); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}

	params := s.leadListParamsFromRequest(r)
	reqCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	page, err := s.leads.SummaryBuckets(reqCtx, dimension, params, q.Get("status"), offset, limit)
	if err != nil {
		if strings.Contains(err.Error(), "unknown summary dimension") {
			writeError(w, http.StatusBadRequest, err.Error())
			return
		}
		log.Printf("leads summary buckets: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load summary buckets")
		return
	}
	s.writeCachedJSON(w, r, page, 15*time.Second)
}

func (s *Server) handleLeadsGeography(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}

	if s.serveFromCache(w, r) {
		return
	}

	filter := s.geoFilterFromRequest(r)
	reqCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	items, err := s.leads.GeographyMix(reqCtx, filter)
	if err != nil {
		log.Printf("geography mix: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load geography")
		return
	}
	total := 0
	for _, item := range items {
		total += item.Count
	}
	s.writeCachedJSON(w, r, map[string]any{
		"items":   items,
		"total":   total,
		"country": filter.Country,
		"city":    filter.City,
	}, 30*time.Second)
}

func (s *Server) handleLeadsAddedSeries(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}

	if s.serveFromCache(w, r) {
		return
	}

	granularity := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("granularity")))
	filter := s.geoFilterFromRequest(r)
	reqCtx, cancel := context.WithTimeout(r.Context(), 12*time.Second)
	defer cancel()

	result, err := s.leads.AddedSeries(reqCtx, granularity, filter)
	if err != nil {
		log.Printf("leads added series: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load added series")
		return
	}
	s.writeCachedJSON(w, r, result, 30*time.Second)
}

func (s *Server) handleLeadsGeoOptions(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !s.requireLeadDataAccess(w, r) {
		return
	}

	if s.serveFromCache(w, r) {
		return
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), 10*time.Second)
	defer cancel()

	q := r.URL.Query()
	kind := strings.TrimSpace(strings.ToLower(q.Get("type")))
	filter := s.geoFilterFromRequest(r)

	if kind == "cities" || filter.Country != "" {
		cities, err := s.leads.ListCities(reqCtx, filter)
		if err != nil {
			log.Printf("list cities: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to load cities")
			return
		}
		s.writeCachedJSON(w, r, map[string]any{
			"type":    "cities",
			"country": filter.Country,
			"items":   cities,
		}, 60*time.Second)
		return
	}

	countries, err := s.leads.ListCountries(reqCtx, filter)
	if err != nil {
		log.Printf("list countries: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load countries")
		return
	}
	s.writeCachedJSON(w, r, map[string]any{
		"type":  "countries",
		"items": countries,
	}, 60*time.Second)
}

func (s *Server) handleDashboard(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Legacy aggregate endpoint — keep restricted; overview/KPIs use /api/leads/summary.
	if !(authUser.Role == RoleSuperadmin || authUser.Role == RoleAnalystTeamLead) {
		writeError(w, http.StatusForbidden, "dashboard aggregate is not available for this role")
		return
	}
	if s.serveFromCache(w, r) {
		return
	}

	reqCtx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()

	overview, err := s.dashboard.Overview(reqCtx)
	if err != nil {
		log.Printf("overview error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load overview")
		return
	}
	leads, err := s.dashboard.RecentLeads(reqCtx, 25)
	if err != nil {
		log.Printf("leads error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load leads")
		return
	}
	teams, err := s.dashboard.Teams(reqCtx)
	if err != nil {
		log.Printf("teams error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load teams")
		return
	}
	users, err := s.users.List(reqCtx, 40)
	if err != nil {
		log.Printf("users error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load users")
		return
	}
	notifs, err := s.dashboard.Notifications(reqCtx, 20)
	if err != nil {
		log.Printf("notifications error: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load notifications")
		return
	}

	s.writeCachedJSON(w, r, DashboardResponse{
		Overview:      overview,
		RecentLeads:   leads,
		Teams:         teams,
		Users:         users,
		Notifications: notifs,
	}, 15*time.Second)
}

func (s *Server) handleTransfers(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	if authUser, ok := userFromContext(r.Context()); ok && (isLeadAnalyst(authUser.Role) || isSalesExecutive(authUser.Role)) {
		writeError(w, http.StatusForbidden, "transfer logs are not available for this role")
		return
	}

	q := r.URL.Query()
	typeParam := strings.TrimSpace(q.Get("type"))
	if typeParam == "" {
		typeParam = "leads"
	}
	if typeParam != "leads" && typeParam != "sales-exec" {
		writeError(w, http.StatusBadRequest, "type must be leads or sales-exec")
		return
	}

	limit := defaultTransferLimit
	if raw := q.Get("limit"); raw != "" {
		if n, err := strconv.Atoi(raw); err == nil {
			limit = n
		}
	}
	limit = clampTransferLimit(limit)

	cursor := q.Get("cursor")
	search := q.Get("q")
	action := q.Get("action")
	ownerID := actorLeadOwnerID(r)
	teamScope := actorLeadTeamID(r)
	if teamScope == "" {
		if authUser, ok := userFromContext(r.Context()); ok && isMainTeamLead(authUser.Role) {
			teamScope = "00000000-0000-0000-0000-000000000000"
		}
	}

	totals, err := s.transfers.Totals(r.Context(), ownerID, teamScope)
	if err != nil {
		log.Printf("transfer totals: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load transfers")
		return
	}

	out := TransferListResult{
		Type:   typeParam,
		Limit:  limit,
		Query:  strings.TrimSpace(search),
		Totals: totals,
	}

	switch typeParam {
	case "sales-exec":
		if ownerID != "" {
			// Creator-scoped roles do not see sales-exec team moves.
			out.Items = []SalesExecTeamTransferLog{}
			out.Total = 0
			out.HasMore = false
			writeJSON(w, http.StatusOK, out)
			return
		}
		items, next, hasMore, total, err := s.transfers.ListSalesExecTeamTransfers(
			r.Context(), search, cursor, limit, teamScope,
		)
		if err != nil {
			if strings.Contains(err.Error(), "invalid cursor") {
				writeError(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			log.Printf("list SE team transfers: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to load sales executive transfers")
			return
		}
		out.Items = items
		out.Total = total
		out.NextCursor = next
		out.HasMore = hasMore
	default:
		items, next, hasMore, total, err := s.transfers.ListLeadTransfers(
			r.Context(), search, action, cursor, limit, ownerID, teamScope,
		)
		if err != nil {
			if strings.Contains(err.Error(), "invalid cursor") {
				writeError(w, http.StatusBadRequest, "invalid cursor")
				return
			}
			if strings.Contains(err.Error(), "invalid action") {
				writeError(w, http.StatusBadRequest, "invalid action")
				return
			}
			log.Printf("list lead transfers: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to load lead transfers")
			return
		}
		mix, mixErr := s.transfers.LeadActionMix(r.Context(), ownerID, teamScope)
		if mixErr != nil {
			log.Printf("lead action mix: %v", mixErr)
			writeError(w, http.StatusInternalServerError, "failed to load lead transfers")
			return
		}
		out.Items = items
		out.Total = total
		out.NextCursor = next
		out.HasMore = hasMore
		out.Action = strings.TrimSpace(action)
		out.ActionMix = mix
	}

	writeJSON(w, http.StatusOK, out)
}
