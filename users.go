package main

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
)

var (
	errUserNotFound          = errors.New("user not found")
	errEmailTaken            = errors.New("email already in use")
	errInvalidCredentials    = errors.New("invalid email or password")
	errPasswordNotSet        = errors.New("password not set for this account")
	errAccountInactive       = errors.New("account is inactive")
	errCannotDeleteSelf      = errors.New("cannot delete your own account")
	errCannotDeactivateSelf  = errors.New("cannot deactivate your own account")
	errLastSuperadmin        = errors.New("cannot delete the last superadmin")
	errLastActiveSuperadmin  = errors.New("cannot deactivate the last active superadmin")
	errDeleteBlocked         = errors.New("user cannot be deleted due to related records")
	errTeamNameRequired      = errors.New("team name is required")
	errNotSalesExecutive     = errors.New("user is not a sales executive")
	errSameTeam              = errors.New("sales executive is already on that team")
	errTeamNotFound          = errors.New("destination team not found")
	errTeamConflict          = errors.New("sales executive team changed — refresh and try again")
	errTransferForbidden     = errors.New("you cannot transfer this sales executive")
)

type UserRecord struct {
	ID                 string
	Email              string
	Name               string
	Role               string
	PasswordHash       *string
	MustResetPassword  bool
	TeamID             *string
	TeamName           *string
	AnalystTeamName    *string
	ManagerID          *string
	ManagerName        *string
	IsOutboundAnalyst  bool
	IsActive           bool
	IsActiveSession    bool
	ActiveSessionSetAt *time.Time
	Image              *string
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

type PublicUser struct {
	ID                 string     `json:"id"`
	Name               string     `json:"name"`
	Email              string     `json:"email"`
	Role               string     `json:"role"`
	RoleLabel          string     `json:"roleLabel"`
	TeamID             *string    `json:"teamId"`
	TeamName           *string    `json:"teamName"`
	AnalystTeamName    *string    `json:"analystTeamName"`
	ManagerID          *string    `json:"managerId"`
	ManagerName        *string    `json:"managerName"`
	IsOutboundAnalyst  bool       `json:"isOutboundAnalyst"`
	IsActive           bool       `json:"isActive"`
	IsActiveSession    bool       `json:"isActiveSession"`
	ActiveSessionSetAt *time.Time `json:"activeSessionSetAt"`
	Image              *string    `json:"image"`
	MustResetPassword  bool       `json:"mustResetPassword"`
	HasPassword        bool       `json:"hasPassword"`
	CreatedAt          time.Time  `json:"createdAt"`
	UpdatedAt          time.Time  `json:"updatedAt"`
}

func (u UserRecord) Public() PublicUser {
	label := u.Role
	if l, ok := roleLabels[u.Role]; ok {
		label = l
	}
	return PublicUser{
		ID:                 u.ID,
		Name:               u.Name,
		Email:              u.Email,
		Role:               u.Role,
		RoleLabel:          label,
		TeamID:             u.TeamID,
		TeamName:           u.TeamName,
		AnalystTeamName:    u.AnalystTeamName,
		ManagerID:          u.ManagerID,
		ManagerName:        u.ManagerName,
		IsOutboundAnalyst:  u.IsOutboundAnalyst,
		IsActive:           u.IsActive,
		IsActiveSession:    u.IsActiveSession,
		ActiveSessionSetAt: u.ActiveSessionSetAt,
		Image:              u.Image,
		MustResetPassword:  u.MustResetPassword,
		HasPassword:        u.PasswordHash != nil && *u.PasswordHash != "",
		CreatedAt:          u.CreatedAt,
		UpdatedAt:          u.UpdatedAt,
	}
}

func (u UserRecord) Auth() AuthUser {
	return AuthUser{ID: u.ID, Email: u.Email, Name: u.Name, Role: u.Role, TeamID: u.TeamID}
}

type UserStore struct {
	pool *pgxpool.Pool
}

func NewUserStore(pool *pgxpool.Pool) *UserStore {
	return &UserStore{pool: pool}
}

func (s *UserStore) FindByEmail(ctx context.Context, email string) (*UserRecord, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.role, u."passwordHash", u."mustResetPassword",
		       u."teamId", t.name, u."analystTeamName",
		       u."managerId", m.name,
		       u."isOutboundAnalyst",
		       COALESCE(u."isActive", TRUE),
		       (u."activeSessionHash" IS NOT NULL AND u."activeSessionHash" <> ''),
		       u."activeSessionSetAt", u.image,
		       u."createdAt", u."updatedAt"
		FROM "User" u
		LEFT JOIN "Team" t ON t.id = u."teamId"
		LEFT JOIN "User" m ON m.id = u."managerId"
		WHERE lower(u.email) = lower($1)
		LIMIT 1`, email)

	var u UserRecord
	err := row.Scan(
		&u.ID, &u.Email, &u.Name, &u.Role, &u.PasswordHash, &u.MustResetPassword,
		&u.TeamID, &u.TeamName, &u.AnalystTeamName,
		&u.ManagerID, &u.ManagerName,
		&u.IsOutboundAnalyst, &u.IsActive, &u.IsActiveSession, &u.ActiveSessionSetAt, &u.Image,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *UserStore) FindByID(ctx context.Context, id string) (*UserRecord, error) {
	row := s.pool.QueryRow(ctx, `
		SELECT u.id, u.email, u.name, u.role, u."passwordHash", u."mustResetPassword",
		       u."teamId", t.name, u."analystTeamName",
		       u."managerId", m.name,
		       u."isOutboundAnalyst",
		       COALESCE(u."isActive", TRUE),
		       (u."activeSessionHash" IS NOT NULL AND u."activeSessionHash" <> ''),
		       u."activeSessionSetAt", u.image,
		       u."createdAt", u."updatedAt"
		FROM "User" u
		LEFT JOIN "Team" t ON t.id = u."teamId"
		LEFT JOIN "User" m ON m.id = u."managerId"
		WHERE u.id = $1
		LIMIT 1`, id)

	var u UserRecord
	err := row.Scan(
		&u.ID, &u.Email, &u.Name, &u.Role, &u.PasswordHash, &u.MustResetPassword,
		&u.TeamID, &u.TeamName, &u.AnalystTeamName,
		&u.ManagerID, &u.ManagerName,
		&u.IsOutboundAnalyst, &u.IsActive, &u.IsActiveSession, &u.ActiveSessionSetAt, &u.Image,
		&u.CreatedAt, &u.UpdatedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, errUserNotFound
	}
	if err != nil {
		return nil, err
	}
	return &u, nil
}

func (s *UserStore) UpdatePasswordHash(ctx context.Context, id, hash string) error {
	_, err := s.pool.Exec(ctx, `
		UPDATE "User"
		SET "passwordHash" = $2, "updatedAt" = NOW()
		WHERE id = $1`, id, hash)
	return err
}

func (s *UserStore) Authenticate(ctx context.Context, email, password string) (*UserRecord, error) {
	user, err := s.FindByEmail(ctx, email)
	if err != nil {
		if errors.Is(err, errUserNotFound) {
			return nil, errInvalidCredentials
		}
		return nil, err
	}
	if !user.IsActive {
		return nil, errAccountInactive
	}
	if user.PasswordHash == nil || *user.PasswordHash == "" {
		return nil, errPasswordNotSet
	}
	ok, upgraded := checkPassword(*user.PasswordHash, password)
	if !ok {
		return nil, errInvalidCredentials
	}
	if upgraded != "" {
		_ = s.UpdatePasswordHash(ctx, user.ID, upgraded)
		user.PasswordHash = &upgraded
	}
	return user, nil
}

type CreateUserInput struct {
	Name     string
	Email    string
	Password string
	Role     string
	TeamID   *string
	TeamName string
}

func (s *UserStore) Create(ctx context.Context, in CreateUserInput) (*UserRecord, error) {
	existing, err := s.FindByEmail(ctx, in.Email)
	if err == nil && existing != nil {
		return nil, errEmailTaken
	}
	if err != nil && !errors.Is(err, errUserNotFound) {
		return nil, err
	}

	hash, err := hashPassword(in.Password)
	if err != nil {
		return nil, err
	}

	teamID := in.TeamID
	teamName := strings.TrimSpace(in.TeamName)
	var analystTeamName *string
	switch in.Role {
	case RoleMainTeamLead:
		if teamName == "" && (teamID == nil || strings.TrimSpace(*teamID) == "") {
			return nil, errTeamNameRequired
		}
	case RoleAnalystTeamLead:
		if teamName == "" {
			return nil, errTeamNameRequired
		}
		name := teamName
		analystTeamName = &name
		teamName = ""
		teamID = nil
	case RoleLeadAnalyst:
		if teamName != "" {
			name := teamName
			analystTeamName = &name
		}
		teamName = ""
		teamID = nil
	default:
		teamName = ""
		if in.Role != RoleSalesExecutive {
			teamID = nil
		}
	}

	id := uuid.NewString()
	now := time.Now().UTC()
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	_, err = tx.Exec(ctx, `
		INSERT INTO "User" (
			id, email, name, role, "passwordHash",
			"mustResetPassword", "isOutboundAnalyst",
			"teamId", "analystTeamName", "createdAt", "updatedAt"
		) VALUES ($1,$2,$3,$4,$5,false,false,$6,$7,$8,$8)`,
		id, in.Email, in.Name, in.Role, hash, teamID, analystTeamName, now,
	)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && pgErr.Code == "23505" {
			return nil, errEmailTaken
		}
		return nil, err
	}

	if in.Role == RoleMainTeamLead && teamName != "" {
		resolvedID, linkErr := s.linkMainTeamLeadToNamedTeam(ctx, tx, id, teamName, now)
		if linkErr != nil {
			return nil, linkErr
		}
		_, err = tx.Exec(ctx, `
			UPDATE "User"
			SET "teamId" = $2, "updatedAt" = $3
			WHERE id = $1`, id, resolvedID, now)
		if err != nil {
			return nil, err
		}
	}

	if err := tx.Commit(ctx); err != nil {
		return nil, err
	}
	return s.FindByID(ctx, id)
}

// linkMainTeamLeadToNamedTeam finds a team by name or creates one, then sets mainTeamLeadId.
func (s *UserStore) linkMainTeamLeadToNamedTeam(
	ctx context.Context,
	tx pgx.Tx,
	userID, teamName string,
	now time.Time,
) (string, error) {
	teamName = strings.TrimSpace(teamName)
	var teamID string
	err := tx.QueryRow(ctx, `
		SELECT id FROM "Team"
		WHERE lower(btrim(name)) = lower(btrim($1))
		LIMIT 1`, teamName).Scan(&teamID)
	if err == nil {
		_, err = tx.Exec(ctx, `
			UPDATE "Team"
			SET "mainTeamLeadId" = $1, "updatedAt" = $2
			WHERE id = $3`, userID, now, teamID)
		if err != nil {
			return "", err
		}
		return teamID, nil
	}
	if !errors.Is(err, pgx.ErrNoRows) {
		return "", err
	}

	teamID = uuid.NewString()
	_, err = tx.Exec(ctx, `
		INSERT INTO "Team" (id, name, "mainTeamLeadId", "createdAt", "updatedAt")
		VALUES ($1, $2, $3, $4, $4)`,
		teamID, teamName, userID, now,
	)
	if err != nil {
		return "", err
	}
	return teamID, nil
}

func (s *UserStore) List(ctx context.Context, limit int) ([]PublicUser, error) {
	if limit <= 0 || limit > 2000 {
		limit = 2000
	}
	rows, err := s.pool.Query(ctx, `
		SELECT u.id, u.email, u.name, u.role, u."passwordHash", u."mustResetPassword",
		       u."teamId", t.name, u."analystTeamName",
		       u."managerId", m.name,
		       u."isOutboundAnalyst",
		       COALESCE(u."isActive", TRUE),
		       (u."activeSessionHash" IS NOT NULL AND u."activeSessionHash" <> ''),
		       u."activeSessionSetAt", u.image,
		       u."createdAt", u."updatedAt"
		FROM "User" u
		LEFT JOIN "Team" t ON t.id = u."teamId"
		LEFT JOIN "User" m ON m.id = u."managerId"
		ORDER BY u.name ASC, u."createdAt" DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]PublicUser, 0)
	for rows.Next() {
		var u UserRecord
		if err := rows.Scan(
			&u.ID, &u.Email, &u.Name, &u.Role, &u.PasswordHash, &u.MustResetPassword,
			&u.TeamID, &u.TeamName, &u.AnalystTeamName,
			&u.ManagerID, &u.ManagerName,
			&u.IsOutboundAnalyst, &u.IsActive, &u.IsActiveSession, &u.ActiveSessionSetAt, &u.Image,
			&u.CreatedAt, &u.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, u.Public())
	}
	return out, rows.Err()
}

func (s *UserStore) Count(ctx context.Context) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `SELECT COUNT(*)::bigint FROM "User"`).Scan(&n)
	return n, err
}

func (s *UserStore) CountByRole(ctx context.Context, role string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint FROM "User" WHERE role = $1`, role).Scan(&n)
	return n, err
}

// CountTeamHeadcount returns active CRM staff count for attrition KPI denominator.
// When teamID is set (Main Team Lead scope), only that team's members are counted.
func (s *UserStore) CountTeamHeadcount(ctx context.Context, teamID string) (int64, error) {
	var n int64
	roles := []string{
		RoleAnalystTeamLead,
		RoleLeadAnalyst,
		RoleMainTeamLead,
		RoleSalesExecutive,
	}
	if teamID != "" {
		err := s.pool.QueryRow(ctx, `
			SELECT COUNT(*)::bigint FROM "User"
			WHERE "teamId" = $1
			  AND role = ANY($2)`, teamID, roles).Scan(&n)
		return n, err
	}
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint FROM "User"
		WHERE role = ANY($1)`, roles).Scan(&n)
	return n, err
}

type UpdateUserInput struct {
	Name              string
	Email             string
	Role              string
	Password          *string
	MustResetPassword *bool
	TeamName          *string
	TeamID            *string
}

// SetActive soft-disables or re-enables an account. Data is preserved; inactive
// users cannot log in and existing sessions are cleared on deactivate.
func (s *UserStore) SetActive(ctx context.Context, id, actorID string, active bool) (*UserRecord, error) {
	id = strings.TrimSpace(id)
	actorID = strings.TrimSpace(actorID)
	if id == "" {
		return nil, errUserNotFound
	}
	if !active && id == actorID {
		return nil, errCannotDeactivateSelf
	}

	user, err := s.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}
	if user.IsActive == active {
		return user, nil
	}

	if !active && user.Role == RoleSuperadmin {
		n, err := s.CountActiveByRole(ctx, RoleSuperadmin)
		if err != nil {
			return nil, err
		}
		if n <= 1 {
			return nil, errLastActiveSuperadmin
		}
	}

	if active {
		_, err = s.pool.Exec(ctx, `
			UPDATE "User"
			SET "isActive" = TRUE, "updatedAt" = NOW()
			WHERE id = $1`, id)
	} else {
		_, err = s.pool.Exec(ctx, `
			UPDATE "User"
			SET "isActive" = FALSE,
			    "activeSessionHash" = NULL,
			    "activeSessionSetAt" = NULL,
			    "updatedAt" = NOW()
			WHERE id = $1`, id)
	}
	if err != nil {
		return nil, err
	}
	return s.FindByID(ctx, id)
}

func (s *UserStore) CountActiveByRole(ctx context.Context, role string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint FROM "User"
		WHERE role = $1 AND COALESCE("isActive", TRUE) = TRUE`, role).Scan(&n)
	return n, err
}

func (s *UserStore) Update(ctx context.Context, id string, in UpdateUserInput) (*UserRecord, error) {
	existing, err := s.FindByID(ctx, id)
	if err != nil {
		return nil, err
	}

	// Email uniqueness (case-insensitive), excluding self.
	if existing.Email != in.Email {
		other, err := s.FindByEmail(ctx, in.Email)
		if err == nil && other != nil && other.ID != id {
			return nil, errEmailTaken
		}
		if err != nil && !errors.Is(err, errUserNotFound) {
			return nil, err
		}
	}

	mustReset := existing.MustResetPassword
	if in.MustResetPassword != nil {
		mustReset = *in.MustResetPassword
	}

	analystTeamName, err := resolveAnalystTeamName(existing, in.Role, in.TeamName)
	if err != nil {
		return nil, err
	}

	teamID := existing.TeamID
	if in.TeamID != nil && strings.TrimSpace(*in.TeamID) != "" && in.Role == RoleSalesExecutive {
		id := strings.TrimSpace(*in.TeamID)
		teamID = &id
	}

	if in.Password != nil && strings.TrimSpace(*in.Password) != "" {
		hash, err := hashPassword(*in.Password)
		if err != nil {
			return nil, err
		}
		_, err = s.pool.Exec(ctx, `
			UPDATE "User"
			SET name = $2,
			    email = $3,
			    role = $4,
			    "passwordHash" = $5,
			    "mustResetPassword" = $6,
			    "analystTeamName" = $7,
			    "teamId" = $8,
			    "activeSessionHash" = NULL,
			    "activeSessionSetAt" = NULL,
			    "updatedAt" = NOW()
			WHERE id = $1`,
			id, in.Name, in.Email, in.Role, hash, mustReset, analystTeamName, teamID,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, errEmailTaken
			}
			return nil, err
		}
	} else {
		_, err = s.pool.Exec(ctx, `
			UPDATE "User"
			SET name = $2,
			    email = $3,
			    role = $4,
			    "mustResetPassword" = $5,
			    "analystTeamName" = $6,
			    "teamId" = $7,
			    "updatedAt" = NOW()
			WHERE id = $1`,
			id, in.Name, in.Email, in.Role, mustReset, analystTeamName, teamID,
		)
		if err != nil {
			var pgErr *pgconn.PgError
			if errors.As(err, &pgErr) && pgErr.Code == "23505" {
				return nil, errEmailTaken
			}
			return nil, err
		}
	}
	return s.FindByID(ctx, id)
}

func resolveAnalystTeamName(existing *UserRecord, role string, teamName *string) (*string, error) {
	current := existing.AnalystTeamName
	if teamName == nil {
		if role == RoleAnalystTeamLead {
			if current == nil || strings.TrimSpace(*current) == "" {
				return nil, errTeamNameRequired
			}
		}
		return current, nil
	}
	name := strings.TrimSpace(*teamName)
	switch role {
	case RoleAnalystTeamLead:
		if name == "" {
			return nil, errTeamNameRequired
		}
		return &name, nil
	case RoleLeadAnalyst:
		if name == "" {
			return nil, nil
		}
		return &name, nil
	default:
		return current, nil
	}
}

// Delete removes a user after preserving leads and neutralizing team ownership.
// Leads are never deleted here — they stay in the CRM until someone
// intentionally deletes the lead. User FKs on Lead are ON DELETE SET NULL.
func (s *UserStore) Delete(ctx context.Context, id, actorID string) error {
	id = strings.TrimSpace(id)
	actorID = strings.TrimSpace(actorID)
	if id == "" {
		return errUserNotFound
	}
	if id == actorID {
		return errCannotDeleteSelf
	}

	user, err := s.FindByID(ctx, id)
	if err != nil {
		return err
	}
	if user.Role == RoleSuperadmin {
		n, err := s.CountByRole(ctx, RoleSuperadmin)
		if err != nil {
			return err
		}
		if n <= 1 {
			return errLastSuperadmin
		}
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	// Keep a living creator so list/KPI analyst grouping still has a user.
	// The FK is SET NULL, so even if this update is skipped the lead survives.
	if actorID != "" {
		if _, err := tx.Exec(ctx, `
			UPDATE "Lead"
			SET "createdById" = $2, "updatedAt" = NOW()
			WHERE "createdById" = $1`, id, actorID); err != nil {
			return err
		}
	}

	// Assigned leads return to the unassigned pool. Qualification, Irrelevant,
	// and Not appropriate flags stay on the lead.
	if _, err := tx.Exec(ctx, `
		UPDATE "Lead"
		SET
			"assignedSalesExecId" = NULL,
			"assignedMainTeamLeadId" = NULL,
			"teamId" = NULL,
			"execAssignedAt" = NULL,
			"execDeadlineAt" = NULL,
			"salesStage" = CASE
				WHEN "salesStage" IN ('WITH_EXECUTIVE', 'WITH_TEAM_LEAD') THEN 'PRE_SALES'
				ELSE "salesStage"
			END,
			"updatedAt" = NOW()
		WHERE "assignedSalesExecId" = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Lead"
		SET "assignedMainTeamLeadId" = NULL, "updatedAt" = NOW()
		WHERE "assignedMainTeamLeadId" = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Lead"
		SET "notAppropriateById" = NULL, "updatedAt" = NOW()
		WHERE "notAppropriateById" = $1`, id); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "User"
		SET "managerId" = NULL, "updatedAt" = NOW()
		WHERE "managerId" = $1`, id); err != nil {
		return err
	}

	// Team.mainTeamLeadId is NOT NULL — reassign to another MTL on the same team.
	rows, err := tx.Query(ctx, `
		SELECT t.id
		FROM "Team" t
		WHERE t."mainTeamLeadId" = $1`, id)
	if err != nil {
		return err
	}
	var teamIDs []string
	for rows.Next() {
		var teamID string
		if err := rows.Scan(&teamID); err != nil {
			rows.Close()
			return err
		}
		teamIDs = append(teamIDs, teamID)
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}
	for _, teamID := range teamIDs {
		var replacement string
		err := tx.QueryRow(ctx, `
			SELECT id FROM "User"
			WHERE role = $1 AND "teamId" = $2 AND id <> $3
			ORDER BY "createdAt" ASC
			LIMIT 1`, RoleMainTeamLead, teamID, id).Scan(&replacement)
		if err != nil {
			return fmt.Errorf("%w: assign another Main Team Lead to the team first", errDeleteBlocked)
		}
		if _, err := tx.Exec(ctx, `
			UPDATE "Team"
			SET "mainTeamLeadId" = $1, "updatedAt" = NOW()
			WHERE id = $2`, replacement, teamID); err != nil {
			return err
		}
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Team"
		SET "analystTeamLeadId" = NULL
		WHERE "analystTeamLeadId" = $1`, id); err != nil {
		return err
	}

	// ApiKey is ON DELETE RESTRICT.
	if _, err := tx.Exec(ctx, `
		DELETE FROM "ApiKey" WHERE "createdById" = $1`, id); err != nil {
		return err
	}

	tag, err := tx.Exec(ctx, `DELETE FROM "User" WHERE id = $1`, id)
	if err != nil {
		var pgErr *pgconn.PgError
		if errors.As(err, &pgErr) && (pgErr.Code == "23503" || pgErr.Code == "23001") {
			return errDeleteBlocked
		}
		return err
	}
	if tag.RowsAffected() == 0 {
		return errUserNotFound
	}
	return tx.Commit(ctx)
}

type TransferSalesExecInput struct {
	SalesExecID    string
	ToTeamID       string
	ExpectedTeamID string // optional optimistic concurrency token (current team)
	ActorID        string
	ActorRole      string
	ActorTeamID    *string
}

type TransferSalesExecResult struct {
	User       PublicUser `json:"user"`
	LeadsMoved int        `json:"leadsMoved"`
	TransferID string     `json:"transferId"`
	FromTeamID *string    `json:"fromTeamId"`
	ToTeamID   string     `json:"toTeamId"`
}

type TeamBrief struct {
	ID   string `json:"id"`
	Name string `json:"name"`
}

func (s *UserStore) TeamIDExists(ctx context.Context, id string) (bool, error) {
	id = strings.TrimSpace(id)
	if id == "" {
		return false, nil
	}
	var found string
	err := s.pool.QueryRow(ctx, `SELECT id FROM "Team" WHERE id = $1`, id).Scan(&found)
	if errors.Is(err, pgx.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

func (s *UserStore) ListTeamsBrief(ctx context.Context) ([]TeamBrief, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, name
		FROM "Team"
		ORDER BY name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := make([]TeamBrief, 0, 64)
	for rows.Next() {
		var t TeamBrief
		if err := rows.Scan(&t.ID, &t.Name); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// TransferSalesExec moves an SE to another team and remaps their assigned leads
// in one transaction (row locks prevent concurrent double-moves).
func (s *UserStore) TransferSalesExec(ctx context.Context, in TransferSalesExecInput) (TransferSalesExecResult, error) {
	empty := TransferSalesExecResult{}
	seID := strings.TrimSpace(in.SalesExecID)
	toTeamID := strings.TrimSpace(in.ToTeamID)
	actorID := strings.TrimSpace(in.ActorID)
	if seID == "" {
		return empty, errUserNotFound
	}
	if toTeamID == "" {
		return empty, errTeamNotFound
	}
	if actorID == "" {
		return empty, fmt.Errorf("actor is required")
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return empty, err
	}
	defer func() { _ = tx.Rollback(ctx) }()

	// Lock SE row first.
	var (
		role        string
		fromTeamID  *string
		name, email string
	)
	err = tx.QueryRow(ctx, `
		SELECT role, "teamId", name, email
		FROM "User"
		WHERE id = $1
		FOR UPDATE`, seID).Scan(&role, &fromTeamID, &name, &email)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, errUserNotFound
		}
		return empty, err
	}
	if role != RoleSalesExecutive {
		return empty, errNotSalesExecutive
	}

	expected := strings.TrimSpace(in.ExpectedTeamID)
	if expected != "" {
		cur := ""
		if fromTeamID != nil {
			cur = strings.TrimSpace(*fromTeamID)
		}
		if cur != expected {
			return empty, errTeamConflict
		}
	}

	// Authorization: MTL may only transfer SEs currently on their team.
	if isMainTeamLead(in.ActorRole) {
		actorTeam := leadTeamScopeID(in.ActorRole, in.ActorTeamID)
		if actorTeam == "" || fromTeamID == nil || strings.TrimSpace(*fromTeamID) != actorTeam {
			return empty, errTransferForbidden
		}
		if toTeamID == actorTeam {
			return empty, errSameTeam
		}
	} else if in.ActorRole != RoleSuperadmin && in.ActorRole != RoleAnalystTeamLead {
		return empty, errTransferForbidden
	}

	fromID := ""
	if fromTeamID != nil {
		fromID = strings.TrimSpace(*fromTeamID)
	}
	if fromID == toTeamID {
		return empty, errSameTeam
	}

	// Lock destination (and source when present) in stable id order to avoid deadlocks.
	lockIDs := []string{toTeamID}
	if fromID != "" {
		lockIDs = append(lockIDs, fromID)
	}
	if len(lockIDs) == 2 && lockIDs[0] > lockIDs[1] {
		lockIDs[0], lockIDs[1] = lockIDs[1], lockIDs[0]
	}
	for _, tid := range lockIDs {
		var ignore string
		if err := tx.QueryRow(ctx, `
			SELECT id FROM "Team" WHERE id = $1 FOR UPDATE`, tid).Scan(&ignore); err != nil {
			if errors.Is(err, pgx.ErrNoRows) {
				return empty, errTeamNotFound
			}
			return empty, err
		}
	}

	var destMTL string
	err = tx.QueryRow(ctx, `
		SELECT "mainTeamLeadId" FROM "Team" WHERE id = $1`, toTeamID).Scan(&destMTL)
	if err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return empty, errTeamNotFound
		}
		return empty, err
	}
	destMTL = strings.TrimSpace(destMTL)
	if destMTL == "" {
		return empty, fmt.Errorf("destination team has no main team lead")
	}

	now := time.Now().UTC()
	tag, err := tx.Exec(ctx, `
		UPDATE "User"
		SET "teamId" = $2, "updatedAt" = $3
		WHERE id = $1 AND role = $4`,
		seID, toTeamID, now, RoleSalesExecutive)
	if err != nil {
		return empty, err
	}
	if tag.RowsAffected() != 1 {
		return empty, errTeamConflict
	}

	leadTag, err := tx.Exec(ctx, `
		UPDATE "Lead"
		SET "teamId" = $2,
		    "assignedMainTeamLeadId" = $3,
		    "updatedAt" = $4
		WHERE "assignedSalesExecId" = $1`,
		seID, toTeamID, destMTL, now)
	if err != nil {
		return empty, err
	}
	leadsMoved := int(leadTag.RowsAffected())

	transferID := uuid.NewString()
	var fromArg any
	if fromID != "" {
		fromArg = fromID
	} else {
		fromArg = nil
	}
	_, err = tx.Exec(ctx, `
		INSERT INTO "SalesExecTeamTransfer" (
			id, "salesExecId", "fromTeamId", "toTeamId", "transferredById", "createdAt"
		) VALUES ($1, $2, $3, $4, $5, $6)`,
		transferID, seID, fromArg, toTeamID, actorID, now,
	)
	if err != nil {
		return empty, err
	}

	if err := tx.Commit(ctx); err != nil {
		return empty, err
	}

	user, err := s.FindByID(ctx, seID)
	if err != nil {
		return empty, err
	}
	var fromPtr *string
	if fromID != "" {
		fromPtr = &fromID
	}
	return TransferSalesExecResult{
		User:       user.Public(),
		LeadsMoved: leadsMoved,
		TransferID: transferID,
		FromTeamID: fromPtr,
		ToTeamID:   toTeamID,
	}, nil
}

func (s *UserStore) CountLeadsAssignedTo(ctx context.Context, salesExecID string) (int64, error) {
	var n int64
	err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::bigint FROM "Lead"
		WHERE "assignedSalesExecId" = $1`, strings.TrimSpace(salesExecID)).Scan(&n)
	return n, err
}
