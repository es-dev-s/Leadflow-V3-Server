package main

import (
	"errors"
	"fmt"
	"log"
	"net/http"
	"strings"
)

// actorMayManageTarget enforces role + team scope for MTL managing SEs.
// Returns false after writing an error response.
func (s *Server) writeTransferError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, errNotSalesExecutive):
		writeError(w, http.StatusBadRequest, "only sales executives can be transferred")
	case errors.Is(err, errSameTeam):
		writeError(w, http.StatusConflict, "sales executive is already on that team")
	case errors.Is(err, errTeamNotFound):
		writeError(w, http.StatusBadRequest, "destination team not found")
	case errors.Is(err, errTeamConflict):
		writeError(w, http.StatusConflict, "sales executive was moved by someone else — refresh and try again")
	case errors.Is(err, errTransferForbidden):
		writeError(w, http.StatusForbidden, "you cannot transfer this sales executive")
	default:
		log.Printf("transfer sales exec: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to transfer sales executive")
	}
}

func (s *Server) writeLeadAnalystTransferError(w http.ResponseWriter, err error) {
	switch {
	case errors.Is(err, errUserNotFound):
		writeError(w, http.StatusNotFound, "user not found")
	case errors.Is(err, errNotLeadAnalyst):
		writeError(w, http.StatusBadRequest, "only lead analysts can be transferred")
	case errors.Is(err, errSameAnalystTeam):
		writeError(w, http.StatusConflict, "lead analyst is already on that team")
	case errors.Is(err, errAnalystTeamNotFound):
		writeError(w, http.StatusBadRequest, "destination analyst team not found")
	case errors.Is(err, errTeamConflict):
		writeError(w, http.StatusConflict, "lead analyst was moved by someone else — refresh and try again")
	case errors.Is(err, errTransferForbidden):
		writeError(w, http.StatusForbidden, "you cannot transfer this lead analyst")
	default:
		log.Printf("transfer lead analyst: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to transfer lead analyst")
	}
}

func (s *Server) actorMayManageTarget(w http.ResponseWriter, actor AuthUser, target *UserRecord) bool {
	if actor.Role == RoleSuperadmin {
		return true
	}
	if !canActOnUser(actor.Role, target.Role) {
		writeError(w, http.StatusForbidden, "you cannot manage this user")
		return false
	}
	if isMainTeamLead(actor.Role) {
		team := leadTeamScopeID(actor.Role, actor.TeamID)
		if team == "" {
			writeError(w, http.StatusForbidden, "your account is not linked to a team")
			return false
		}
		if target.Role != RoleSalesExecutive || !sameTeamID(target.TeamID, &team) {
			writeError(w, http.StatusNotFound, "user not found")
			return false
		}
	}
	return true
}

type TransferSalesExecRequest struct {
	ToTeamID       string  `json:"toTeamId"`
	ExpectedTeamID *string `json:"expectedTeamId"`
}

func (s *Server) transferSalesExec(w http.ResponseWriter, r *http.Request, id string, actor AuthUser) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !(actor.Role == RoleSuperadmin || actor.Role == RoleAnalystTeamLead || isMainTeamLead(actor.Role)) {
		writeError(w, http.StatusForbidden, "you do not have permission to transfer sales executives")
		return
	}

	var req TransferSalesExecRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	toTeamID := strings.TrimSpace(req.ToTeamID)
	if toTeamID == "" {
		writeError(w, http.StatusBadRequest, "toTeamId is required")
		return
	}
	expected := ""
	if req.ExpectedTeamID != nil {
		expected = strings.TrimSpace(*req.ExpectedTeamID)
	}

	result, err := s.users.TransferSalesExec(r.Context(), TransferSalesExecInput{
		SalesExecID:    id,
		ToTeamID:       toTeamID,
		ExpectedTeamID: expected,
		ActorID:        actor.ID,
		ActorRole:      actor.Role,
		ActorTeamID:    actor.TeamID,
	})
	if err != nil {
		s.writeTransferError(w, err)
		return
	}

	fromLabel, toLabel := "—", "—"
	if result.FromTeamID != nil {
		if teams, err := s.users.ListTeamsBrief(r.Context()); err == nil {
			for _, t := range teams {
				if result.FromTeamID != nil && t.ID == *result.FromTeamID {
					fromLabel = t.Name
				}
				if t.ID == result.ToTeamID {
					toLabel = t.Name
				}
			}
		}
	} else if teams, err := s.users.ListTeamsBrief(r.Context()); err == nil {
		for _, t := range teams {
			if t.ID == result.ToTeamID {
				toLabel = t.Name
				break
			}
		}
	}
	s.notifications.NotifyStaff(
		r.Context(),
		actor.ID,
		NotifSETeamTransfer,
		"SE transferred",
		fmt.Sprintf("%s · %s → %s", result.User.Name, fromLabel, toLabel),
		nil,
		result.User.ID,
	)

	writeJSON(w, http.StatusOK, result)
}

type TransferLeadAnalystRequest struct {
	ToTeamName       string  `json:"toTeamName"`
	ToLeadID         string  `json:"toLeadId"`
	ExpectedTeamName *string `json:"expectedTeamName"`
}

func (s *Server) transferLeadAnalyst(w http.ResponseWriter, r *http.Request, id string, actor AuthUser) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !(actor.Role == RoleSuperadmin || actor.Role == RoleAnalystTeamLead) {
		writeError(w, http.StatusForbidden, "you do not have permission to transfer lead analysts")
		return
	}

	target, err := s.users.FindByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("transfer lead analyst load: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to transfer lead analyst")
		return
	}
	if !s.actorMayManageTarget(w, actor, target) {
		return
	}

	var req TransferLeadAnalystRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	toTeamName := strings.TrimSpace(req.ToTeamName)
	toLeadID := strings.TrimSpace(req.ToLeadID)
	if toTeamName == "" && toLeadID == "" {
		writeError(w, http.StatusBadRequest, "toTeamName or toLeadId is required")
		return
	}
	expected := ""
	if req.ExpectedTeamName != nil {
		expected = strings.TrimSpace(*req.ExpectedTeamName)
	}

	result, err := s.users.TransferLeadAnalyst(r.Context(), TransferLeadAnalystInput{
		LeadAnalystID:    id,
		ToTeamName:       toTeamName,
		ToLeadID:         toLeadID,
		ExpectedTeamName: expected,
		ActorID:          actor.ID,
		ActorRole:        actor.Role,
	})
	if err != nil {
		s.writeLeadAnalystTransferError(w, err)
		return
	}

	writeJSON(w, http.StatusOK, result)
}

func (s *Server) handleAnalystTeams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if !(authUser.Role == RoleSuperadmin || authUser.Role == RoleAnalystTeamLead) {
		writeError(w, http.StatusForbidden, "you do not have permission to list analyst teams")
		return
	}
	teams, err := s.users.ListAnalystTeamsBrief(r.Context())
	if err != nil {
		log.Printf("list analyst teams: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list analyst teams")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}

func (s *Server) handleTeams(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "authentication required")
		return
	}

	scope := strings.ToLower(strings.TrimSpace(r.URL.Query().Get("scope")))
	if scope == "analyst" {
		if !(authUser.Role == RoleSuperadmin || authUser.Role == RoleAnalystTeamLead) {
			writeError(w, http.StatusForbidden, "you do not have permission to list analyst teams")
			return
		}
		teams, err := s.users.ListAnalystTeamsBrief(r.Context())
		if err != nil {
			log.Printf("list analyst teams: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to list analyst teams")
			return
		}
		writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
		return
	}

	if !(authUser.Role == RoleSuperadmin || authUser.Role == RoleAnalystTeamLead || isMainTeamLead(authUser.Role)) {
		writeError(w, http.StatusForbidden, "you do not have permission to list teams")
		return
	}
	teams, err := s.users.ListTeamsBrief(r.Context())
	if err != nil {
		log.Printf("list teams: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to list teams")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"teams": teams})
}
