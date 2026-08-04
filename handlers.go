package main

import (
	"context"
	"log"
	"net/http"
	"time"
)

type Server struct {
	users         *UserStore
	dashboard     *DashboardStore
	leads         *LeadStore
	transfers     *TransferStore
	notifications *NotificationStore
	tokens        *TokenService
	authCookie    AuthCookieConfig
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
		ServiceLine:         q.Get("serviceLine"),
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
	// Token is returned for API/automation clients (Authorization header).
	// Browsers should rely on the HttpOnly cookie and must not persist this.
	Token     string     `json:"token,omitempty"`
	ExpiresAt time.Time  `json:"expiresAt"`
	User      PublicUser `json:"user"`
}
