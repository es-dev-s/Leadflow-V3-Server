package main

import (
	"errors"
	"log"
	"net/http"
	"strings"
)

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
	if len(parts) == 2 && parts[1] == "active" {
		s.setUserActive(w, r, id, *authUser)
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

func (s *Server) setUserActive(w http.ResponseWriter, r *http.Request, id string, actor AuthUser) {
	if r.Method != http.MethodPatch {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if !canManageUsers(actor.Role) {
		writeError(w, http.StatusForbidden, "you do not have permission to manage users")
		return
	}

	var req struct {
		IsActive *bool `json:"isActive"`
	}
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	if req.IsActive == nil {
		writeError(w, http.StatusBadRequest, "isActive is required")
		return
	}

	existing, err := s.users.FindByID(r.Context(), id)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			writeError(w, http.StatusNotFound, "user not found")
			return
		}
		log.Printf("set user active load: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update user")
		return
	}
	if !s.actorMayManageTarget(w, actor, existing) {
		return
	}

	user, err := s.users.SetActive(r.Context(), id, actor.ID, *req.IsActive)
	if err != nil {
		switch {
		case errors.Is(err, errUserNotFound):
			writeError(w, http.StatusNotFound, "user not found")
		case errors.Is(err, errCannotDeactivateSelf):
			writeError(w, http.StatusBadRequest, "cannot deactivate your own account")
		case errors.Is(err, errLastActiveSuperadmin):
			writeError(w, http.StatusConflict, "cannot deactivate the last active superadmin")
		default:
			log.Printf("set user active: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to update user")
		}
		return
	}

	s.emitUser(EvtUserUpdated, user.ID, "")
	writeJSON(w, http.StatusOK, map[string]any{"user": user.Public()})
}
