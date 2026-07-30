package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"
)

type Server struct {
	users         *UserStore
	dashboard     *DashboardStore
	leads         *LeadStore
	transfers     *TransferStore
	notifications *NotificationStore
	tokens        *TokenService
	loginGate     *loginLimiter
	poolPing      func(context.Context) error
	hub           *RealtimeHub
	respCache     ResponseCacher
	telemetry     *TelemetryEmitter
	uploads       *UploadStore
}

// actorLeadOwnerID returns a non-empty creator ID when the actor is creator-scoped.
func actorLeadOwnerID(r *http.Request) string {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		return ""
	}
	return leadDataOwnerID(authUser.Role, authUser.ID)
}

// actorLeadTeamID returns a non-empty team ID when the actor is team-scoped (Main Team Lead).
func actorLeadTeamID(r *http.Request) string {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		return ""
	}
	return leadTeamScopeID(authUser.Role, authUser.TeamID)
}

// actorLeadSalesExecID returns a non-empty SE ID when the actor is assignee-scoped.
func actorLeadSalesExecID(r *http.Request) string {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		return ""
	}
	return leadSalesExecScopeID(authUser.Role, authUser.ID)
}

// applyActorLeadScope forces creator / team / assignee scope for LA / MTL / SE.
func applyActorLeadScope(r *http.Request, params *LeadListParams) {
	if owner := actorLeadOwnerID(r); owner != "" {
		params.AnalystID = owner
	}
	if seID := actorLeadSalesExecID(r); seID != "" {
		params.SalesExecID = seID
	}
	if teamID := actorLeadTeamID(r); teamID != "" {
		params.TeamID = teamID
		return
	}
	if authUser, ok := userFromContext(r.Context()); ok && isMainTeamLead(authUser.Role) {
		// MTL without a linked team must not see workspace-wide leads.
		params.TeamID = "00000000-0000-0000-0000-000000000000"
	}
}

func (s *Server) leadListParamsFromRequest(r *http.Request) LeadListParams {
	q := r.URL.Query()
	params := LeadListParams{
		Filter:              q.Get("filter"),
		Country:             q.Get("country"),
		City:                q.Get("city"),
		TeamID:              q.Get("teamId"),
		AnalystID:           q.Get("analystId"),
		SalesExecID:         q.Get("salesExecId"),
		ManagerID:           q.Get("managerId"),
		Source:              q.Get("source"),
		Portal:              q.Get("portal"),
		MetaProfile:         q.Get("metaProfile"),
		Status:              q.Get("status"),
		Stage:               q.Get("stage"),
		QualificationReason: firstNonEmpty(q.Get("reason"), q.Get("qualificationReason")),
		AddedFrom:           q.Get("addedFrom"),
		AddedTo:             q.Get("addedTo"),
	}
	applyActorLeadScope(r, &params)
	return params
}

func (s *Server) geoFilterFromRequest(r *http.Request) GeoFilter {
	filter := parseGeoFilter(r.URL.Query().Get("country"), r.URL.Query().Get("city"))
	filter.CreatedByID = actorLeadOwnerID(r)
	filter.TeamID = actorLeadTeamID(r)
	filter.SalesExecID = actorLeadSalesExecID(r)
	if filter.TeamID == "" {
		if authUser, ok := userFromContext(r.Context()); ok && isMainTeamLead(authUser.Role) {
			filter.TeamID = "00000000-0000-0000-0000-000000000000"
		}
	}
	return filter
}

// requireLeadDataAccess blocks Support (and unknown roles) from any lead surface.
// Returns false after writing an error response.
func (s *Server) requireLeadDataAccess(w http.ResponseWriter, r *http.Request) bool {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return false
	}
	if !canViewLeadData(authUser.Role) {
		writeError(w, http.StatusForbidden, "lead data is not available for this role")
		return false
	}
	return true
}

// requireLeadAccess enforces creator / team / assignee ownership for scoped roles.
// Returns false after writing an error response.
func (s *Server) requireLeadAccess(w http.ResponseWriter, r *http.Request, leadID string) bool {
	if !s.requireLeadDataAccess(w, r) {
		return false
	}
	owner := actorLeadOwnerID(r)
	if owner != "" {
		ok, err := s.leads.IsCreatedBy(r.Context(), leadID, owner)
		if err != nil {
			log.Printf("lead ownership check: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to authorize lead access")
			return false
		}
		if !ok {
			writeError(w, http.StatusNotFound, "lead not found")
			return false
		}
		return true
	}

	if seID := actorLeadSalesExecID(r); seID != "" {
		ok, err := s.leads.IsAssignedToSalesExec(r.Context(), leadID, seID)
		if err != nil {
			log.Printf("lead assignee check: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to authorize lead access")
			return false
		}
		if !ok {
			writeError(w, http.StatusNotFound, "lead not found")
			return false
		}
		return true
	}

	teamID := actorLeadTeamID(r)
	if teamID == "" {
		if authUser, ok := userFromContext(r.Context()); ok && isMainTeamLead(authUser.Role) {
			writeError(w, http.StatusNotFound, "lead not found")
			return false
		}
		return true
	}
	ok, err := s.leads.IsOnTeam(r.Context(), leadID, teamID)
	if err != nil {
		log.Printf("lead team check: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to authorize lead access")
		return false
	}
	if !ok {
		writeError(w, http.StatusNotFound, "lead not found")
		return false
	}
	return true
}

type LoginRequest struct {
	Email    string `json:"email"`
	Password string `json:"password"`
}

type CreateUserRequest struct {
	Name     string  `json:"name"`
	Email    string  `json:"email"`
	Password string  `json:"password"`
	Role     string  `json:"role"`
	TeamID   *string `json:"teamId"`
	TeamName *string `json:"teamName"`
}

type UpdateUserRequest struct {
	Name              string  `json:"name"`
	Email             string  `json:"email"`
	Role              string  `json:"role"`
	Password          *string `json:"password"`
	MustResetPassword *bool   `json:"mustResetPassword"`
}

type AuthResponse struct {
	Token     string     `json:"token"`
	ExpiresAt time.Time  `json:"expiresAt"`
	User      PublicUser `json:"user"`
}

func (s *Server) handleHealth(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	dbStatus := "ok"
	if err := s.poolPing(r.Context()); err != nil {
		dbStatus = "unreachable"
	}
	status := "ok"
	if dbStatus != "ok" {
		status = "degraded"
	}
	writeJSON(w, http.StatusOK, map[string]string{
		"status":   status,
		"service":  "leadflow-backend",
		"database": dbStatus,
		"time":     time.Now().UTC().Format(time.RFC3339),
	})
}

func (s *Server) handleLogin(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}

	var req LoginRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	v := &ValidationError{}
	req.Email = requireEmail(v, "email", req.Email)
	// Login accepts any non-empty password length; creation still enforces strength.
	if strings.TrimSpace(req.Password) == "" {
		v.Add("password", "password is required")
	} else if utf8.RuneCountInString(req.Password) > 128 {
		v.Add("password", "password must be at most 128 characters")
	}
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}

	ip := clientIP(r)
	gateKey := ip + "|" + strings.ToLower(req.Email)

	user, err := s.users.Authenticate(r.Context(), req.Email, req.Password)
	if err != nil {
		// Count failures only so concurrent valid sessions are never rate-limited.
		if s.loginGate != nil && !s.loginGate.allow(gateKey) {
			writeError(w, http.StatusTooManyRequests, "too many login attempts — try again shortly")
			return
		}
		switch {
		case errors.Is(err, errInvalidCredentials):
			writeError(w, http.StatusUnauthorized, "invalid email or password")
		case errors.Is(err, errPasswordNotSet):
			writeError(w, http.StatusForbidden, "password not set — ask a superadmin to set one")
		default:
			log.Printf("login error: %v", err)
			writeError(w, http.StatusInternalServerError, "login failed")
		}
		return
	}

	token, expires, err := s.tokens.Issue(user.Auth())
	if err != nil {
		writeError(w, http.StatusInternalServerError, "could not issue token")
		return
	}

	writeJSON(w, http.StatusOK, AuthResponse{
		Token:     token,
		ExpiresAt: expires,
		User:      user.Public(),
	})
}

func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	user, err := s.users.FindByID(r.Context(), authUser.ID)
	if err != nil {
		writeError(w, http.StatusUnauthorized, "user no longer exists")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"user": user.Public()})
}

func (s *Server) handleRoles(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	// Create/edit pickers get only roles this actor may assign.
	writeJSON(w, http.StatusOK, map[string]any{
		"roles":     listCreatableRoles(authUser.Role),
		"allRoles":  listRoles(),
		"canManage": canManageUsers(authUser.Role),
	})
}

func (s *Server) handleUsers(w http.ResponseWriter, r *http.Request) {
	switch r.Method {
	case http.MethodGet:
		s.listUsers(w, r)
	case http.MethodPost:
		authUser, ok := userFromContext(r.Context())
		if !ok {
			writeError(w, http.StatusUnauthorized, "authentication required")
			return
		}
		if !canManageUsers(authUser.Role) {
			writeError(w, http.StatusForbidden, "you do not have permission to create users")
			return
		}
		s.createUser(w, r, *authUser)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) listUsers(w http.ResponseWriter, r *http.Request) {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !canViewUsers(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to view users")
		return
	}

	users, err := s.users.List(r.Context(), 2000)
	if err != nil {
		log.Printf("list users: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list users")
		return
	}

	if authUser.Role == RoleSuperadmin {
		total, err := s.users.Count(r.Context())
		if err != nil {
			log.Printf("count users: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to list users")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{
			"users":     users,
			"total":     total,
			"canManage": true,
		})
		return
	}

	allowed := roleSet(visibleUserRoles(authUser.Role))
	if len(allowed) == 0 {
		writeJSON(w, http.StatusOK, map[string]any{
			"users":     []PublicUser{},
			"total":     0,
			"canManage": false,
		})
		return
	}

	teamScope := leadTeamScopeID(authUser.Role, authUser.TeamID)
	filtered := make([]PublicUser, 0, len(users))
	for _, u := range users {
		if _, ok := allowed[u.Role]; !ok {
			continue
		}
		if teamScope != "" {
			if u.TeamID == nil || strings.TrimSpace(*u.TeamID) != teamScope {
				continue
			}
		}
		filtered = append(filtered, u)
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"users":     filtered,
		"total":     len(filtered),
		"canManage": canManageUsers(authUser.Role),
	})
}

func (s *Server) createUser(w http.ResponseWriter, r *http.Request, actor AuthUser) {
	var req CreateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	// Main Team Leads may only create Sales Executives on their own team.
	if isMainTeamLead(actor.Role) {
		team := leadTeamScopeID(actor.Role, actor.TeamID)
		if team == "" {
			writeError(w, http.StatusBadRequest, "your account is not linked to a team")
			return
		}
		requested := strings.TrimSpace(req.Role)
		if requested != "" && requested != RoleSalesExecutive {
			writeError(w, http.StatusForbidden, "you can only create sales executives")
			return
		}
		req.Role = RoleSalesExecutive
		req.TeamID = &team
		req.TeamName = nil
	}

	v := &ValidationError{}
	req.Name = requireString(v, "name", req.Name, 2, 120)
	req.Email = requireEmail(v, "email", req.Email)
	req.Password = requirePassword(v, "password", req.Password)
	req.Role = requireRole(v, "role", req.Role)
	if req.TeamID != nil {
		trimmed := strings.TrimSpace(*req.TeamID)
		if trimmed == "" {
			req.TeamID = nil
		} else {
			req.TeamID = &trimmed
		}
	}
	teamName := ""
	if req.TeamName != nil {
		teamName = strings.TrimSpace(*req.TeamName)
	}
	if req.Role == RoleMainTeamLead {
		if teamName == "" && req.TeamID == nil {
			v.Add("teamName", "team name is required for Main Team Lead")
		} else if teamName != "" {
			teamName = requireString(v, "teamName", teamName, 2, 120)
		}
	} else {
		teamName = ""
	}
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}
	if !canCreateRole(actor.Role, req.Role) {
		writeError(w, http.StatusForbidden, "you cannot create users with this role")
		return
	}

	user, err := s.users.Create(r.Context(), CreateUserInput{
		Name:     req.Name,
		Email:    req.Email,
		Password: req.Password,
		Role:     req.Role,
		TeamID:   req.TeamID,
		TeamName: teamName,
	})
	if err != nil {
		if errors.Is(err, errEmailTaken) {
			writeError(w, http.StatusConflict, "email already in use")
			return
		}
		if errors.Is(err, errTeamNameRequired) {
			writeError(w, http.StatusBadRequest, "team name is required for Main Team Lead")
			return
		}
		log.Printf("create user: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create user")
		return
	}

	s.emitUser(EvtUserCreated, user.ID, actor.ID)
	writeJSON(w, http.StatusCreated, map[string]any{
		"user":              user.Public(),
		"temporaryPassword": req.Password,
	})
}

func (s *Server) handleUserByID(w http.ResponseWriter, r *http.Request) {
	rest := strings.TrimPrefix(r.URL.Path, "/api/users/")
	rest = strings.Trim(rest, "/")
	if rest == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}
	parts := strings.Split(rest, "/")
	id := strings.TrimSpace(parts[0])
	if id == "" {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	if len(parts) == 2 && parts[1] == "transfer-team" {
		s.transferSalesExec(w, r, id, *authUser)
		return
	}
	if len(parts) != 1 {
		writeError(w, http.StatusNotFound, "not found")
		return
	}

	if !canManageUsers(authUser.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to manage users")
		return
	}

	switch r.Method {
	case http.MethodPatch:
		s.updateUser(w, r, id, *authUser)
	case http.MethodDelete:
		s.deleteUser(w, r, id, *authUser)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

func (s *Server) updateUser(w http.ResponseWriter, r *http.Request, id string, actor AuthUser) {
	var req UpdateUserRequest
	if err := decodeJSON(r, &req); err != nil {
		log.Printf("update user JSON: %v", err)
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}

	existing, err := s.users.FindByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("update user load: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	if !s.actorMayManageTarget(w, actor, existing) {
		return
	}
	if isMainTeamLead(actor.Role) {
		req.Role = RoleSalesExecutive
	}

	v := &ValidationError{}
	req.Name = requireString(v, "name", req.Name, 2, 120)
	req.Email = requireEmail(v, "email", req.Email)
	req.Role = requireRole(v, "role", req.Role)
	var password *string
	if req.Password != nil && strings.TrimSpace(*req.Password) != "" {
		pwd := requirePassword(v, "password", *req.Password)
		password = &pwd
	}
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}

	if !canCreateRole(actor.Role, req.Role) {
		writeError(w, http.StatusForbidden, "you cannot assign this role")
		return
	}
	if existing.Role == RoleSuperadmin && req.Role != RoleSuperadmin {
		n, err := s.users.CountByRole(r.Context(), RoleSuperadmin)
		if err != nil {
			log.Printf("count superadmin: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
			return
		}
		if n <= 1 {
			writeError(w, http.StatusConflict, "cannot demote the last superadmin")
			return
		}
	}

	user, err := s.users.Update(r.Context(), id, UpdateUserInput{
		Name:              req.Name,
		Email:             req.Email,
		Role:              req.Role,
		Password:          password,
		MustResetPassword: req.MustResetPassword,
	})
	if err != nil {
		switch {
		case errors.Is(err, errUserNotFound):
			writeError(w, http.StatusNotFound, "user not found")
		case errors.Is(err, errEmailTaken):
			writeError(w, http.StatusConflict, "email already in use")
		default:
			log.Printf("update user: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
		}
		return
	}

	s.emitUser(EvtUserUpdated, user.ID, "")
	out := map[string]any{"user": user.Public()}
	if password != nil {
		out["temporaryPassword"] = *password
	}
	writeJSON(w, http.StatusOK, out)
}

func (s *Server) deleteUser(w http.ResponseWriter, r *http.Request, id string, actor AuthUser) {
	existing, err := s.users.FindByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("delete user load: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to delete user")
		return
	}
	if !s.actorMayManageTarget(w, actor, existing) {
		return
	}

	err = s.users.Delete(r.Context(), id, actor.ID)
	if err != nil {
		switch {
		case errors.Is(err, errUserNotFound):
			writeError(w, http.StatusNotFound, "user not found")
		case errors.Is(err, errCannotDeleteSelf):
			writeError(w, http.StatusBadRequest, "cannot delete your own account")
		case errors.Is(err, errLastSuperadmin):
			writeError(w, http.StatusConflict, "cannot delete the last superadmin")
		case errors.Is(err, errDeleteBlocked):
			writeError(w, http.StatusConflict, "user cannot be deleted while related records still reference them")
		default:
			log.Printf("delete user: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to delete user")
		}
		return
	}
	s.emitUser(EvtUserDeleted, id, actor.ID)
	writeJSON(w, http.StatusOK, map[string]any{"deleted": true, "id": id})
}

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
	// bumps the cache generation, so entries never outlive a data change; the
	// short TTL only bounds the viewer-specific "new" tag staleness.
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

	viewerID := ""
	analystID := q.Get("analystId")
	teamID := q.Get("teamId")
	salesExecID := q.Get("salesExecId")
	if authUser, ok := userFromContext(r.Context()); ok {
		viewerID = authUser.ID
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
		ViewerID:            viewerID,
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
	layouts := []string{
		"2006-01-02T15:04",
		"2006-01-02T15:04:05",
		time.RFC3339,
		"2006-01-02 15:04",
		"2006-01-02 15:04:05",
	}
	var lastErr error
	for _, layout := range layouts {
		parsed, err := time.ParseInLocation(layout, trimmed, time.Local)
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
	email := strings.TrimSpace(q.Get("email"))
	excludeID := strings.TrimSpace(q.Get("excludeId"))

	match, err := s.leads.FindDuplicateByContact(r.Context(), phone, email, excludeID)
	if err != nil {
		log.Printf("contact lookup: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to look up contact")
		return
	}
	if match == nil {
		writeJSON(w, http.StatusOK, map[string]any{"exists": false})
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"exists":    true,
		"id":        match.ID,
		"leadName":  match.LeadName,
		"teamName":  teamLabelFromMatch(match),
		"matchedOn": match.MatchedOn,
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
	req.FullName = requireString(v, "fullName", req.FullName, 2, 200)
	req.Source = requireString(v, "source", req.Source, 2, 80)
	req.QualificationStatus = requireString(v, "qualificationStatus", req.QualificationStatus, 2, 40)
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}

	email := optionalTrimmed(req.Email)
	phone := optionalTrimmed(req.Phone)
	emailVal, phoneVal := "", ""
	if email != nil {
		emailVal = *email
	}
	if phone != nil {
		phoneVal = *phone
	}
	if dup, err := s.leads.FindDuplicateByContact(r.Context(), phoneVal, emailVal, ""); err != nil {
		log.Printf("create lead duplicate check: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to create lead")
		return
	} else if dup != nil {
		writeError(w, http.StatusConflict, "Already in "+teamLabelFromMatch(dup))
		return
	}

	var createdAt *time.Time
	if req.CreatedAt != nil && strings.TrimSpace(*req.CreatedAt) != "" {
		raw := strings.TrimSpace(*req.CreatedAt)
		// Date-only is the product format; accept a few legacy datetime shapes too.
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			parsed, err = time.Parse("2006-01-02T15:04", raw)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid createdAt date")
			return
		}
		// Persist as calendar date at local midnight.
		day := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.Local)
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
	case "assign", "summary", "geography", "geo-options", "contact-lookup", "pipeline", "added-series":
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
		if authUser, ok := userFromContext(r.Context()); ok {
			if markErr := s.leads.MarkLeadViewed(r.Context(), authUser.ID, id); markErr != nil {
				log.Printf("mark lead viewed: %v", markErr)
			}
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
	updated, err := s.leads.UpdateQualificationStatus(r.Context(), id, status)
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
	req.FullName = requireString(v, "fullName", req.FullName, 2, 200)
	req.Source = requireString(v, "source", req.Source, 2, 80)
	req.QualificationStatus = requireString(v, "qualificationStatus", req.QualificationStatus, 2, 40)
	if v.HasErrors() {
		writeValidationError(w, v)
		return
	}

	email := optionalTrimmed(req.Email)
	phone := optionalTrimmed(req.Phone)
	emailVal, phoneVal := "", ""
	if email != nil {
		emailVal = *email
	}
	if phone != nil {
		phoneVal = *phone
	}
	if dup, err := s.leads.FindDuplicateByContact(r.Context(), phoneVal, emailVal, id); err != nil {
		log.Printf("update lead duplicate check: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update lead")
		return
	} else if dup != nil {
		writeError(w, http.StatusConflict, "Already in "+teamLabelFromMatch(dup))
		return
	}

	var createdAt *time.Time
	if req.CreatedAt != nil && strings.TrimSpace(*req.CreatedAt) != "" {
		raw := strings.TrimSpace(*req.CreatedAt)
		parsed, err := time.ParseInLocation("2006-01-02", raw, time.Local)
		if err != nil {
			parsed, err = time.Parse(time.RFC3339, raw)
		}
		if err != nil {
			parsed, err = time.Parse("2006-01-02T15:04", raw)
		}
		if err != nil {
			writeError(w, http.StatusBadRequest, "invalid createdAt date")
			return
		}
		day := time.Date(parsed.Year(), parsed.Month(), parsed.Day(), 0, 0, 0, 0, time.Local)
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

	summary, err := s.leads.Summary(reqCtx, params, hideActive)
	if err != nil {
		log.Printf("leads summary: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to load leads summary")
		return
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

	if authUser, ok := userFromContext(r.Context()); ok && (isLeadAnalyst(authUser.Role) || isSalesExecutive(authUser.Role) || isSupport(authUser.Role)) {
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
