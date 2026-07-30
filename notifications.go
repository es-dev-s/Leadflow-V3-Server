package main

import (
	"context"
	"fmt"
	"log"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5/pgxpool"
)

const (
	NotifLeadAdded     = "LEAD_ADDED"
	NotifLeadUpdated   = "LEAD_UPDATED"
	NotifLeadDeleted   = "LEAD_DELETED"
	NotifLeadAssigned  = "LEAD_ASSIGNED"
	NotifLeadTransfer  = "LEAD_TRANSFER"
	NotifSETeamTransfer = "SE_TEAM_TRANSFER"
)

type AppNotification struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      string    `json:"body"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"createdAt"`
	LeadID    *string   `json:"leadId,omitempty"`
	Href      string    `json:"href"`
}

type NotificationListResponse struct {
	Items       []AppNotification `json:"items"`
	UnreadCount int               `json:"unreadCount"`
}

type NotificationStore struct {
	pool *pgxpool.Pool
}

func NewNotificationStore(pool *pgxpool.Pool) *NotificationStore {
	return &NotificationStore{pool: pool}
}

func notificationHref(kind string, leadID *string, query string) string {
	q := strings.TrimSpace(query)
	switch kind {
	case NotifLeadTransfer:
		href := "/transfers?tab=leads"
		if q != "" {
			href += "&q=" + url.QueryEscape(q)
		}
		return href
	case NotifSETeamTransfer:
		href := "/transfers?tab=sales-exec"
		if q != "" {
			href += "&q=" + url.QueryEscape(q)
		}
		return href
	case NotifLeadDeleted:
		return "/leads"
	default:
		if leadID != nil && strings.TrimSpace(*leadID) != "" {
			return "/leads?lead=" + url.QueryEscape(strings.TrimSpace(*leadID))
		}
		return "/leads"
	}
}

func (s *NotificationStore) staffRecipientIDs(ctx context.Context, excludeID string) ([]string, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id FROM "User"
		WHERE role IN ($1, $2, $3)
		ORDER BY "createdAt" ASC`,
		RoleSuperadmin, RoleMainTeamLead, RoleAnalystTeamLead,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	excludeID = strings.TrimSpace(excludeID)
	out := make([]string, 0, 32)
	seen := map[string]struct{}{}
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		if id == "" || id == excludeID {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, rows.Err()
}

func mergeRecipientIDs(base []string, extra ...string) []string {
	seen := map[string]struct{}{}
	out := make([]string, 0, len(base)+len(extra))
	for _, id := range append(base, extra...) {
		id = strings.TrimSpace(id)
		if id == "" {
			continue
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out
}

func (s *NotificationStore) CreateMany(
	ctx context.Context,
	recipientIDs []string,
	kind, title, body string,
	leadID *string,
) {
	if len(recipientIDs) == 0 {
		return
	}
	kind = strings.TrimSpace(kind)
	title = strings.TrimSpace(title)
	body = strings.TrimSpace(body)
	if kind == "" || title == "" {
		return
	}
	if utf8Len(title) > 120 {
		title = string([]rune(title)[:117]) + "…"
	}
	if utf8Len(body) > 180 {
		body = string([]rune(body)[:177]) + "…"
	}

	now := time.Now().UTC()
	const batchSize = 100
	for start := 0; start < len(recipientIDs); start += batchSize {
		end := start + batchSize
		if end > len(recipientIDs) {
			end = len(recipientIDs)
		}
		chunk := recipientIDs[start:end]
		values := make([]string, 0, len(chunk))
		args := make([]any, 0, len(chunk)*7)
		for _, recipientID := range chunk {
			recipientID = strings.TrimSpace(recipientID)
			if recipientID == "" {
				continue
			}
			n := len(args)
			values = append(values, fmt.Sprintf(
				"($%d, $%d, $%d, $%d, $%d, $%d, false, $%d)",
				n+1, n+2, n+3, n+4, n+5, n+6, n+7,
			))
			args = append(args, uuid.NewString(), recipientID, kind, leadID, title, nullIfEmpty(body), now)
		}
		if len(values) == 0 {
			continue
		}
		_, err := s.pool.Exec(ctx, `
			INSERT INTO "Notification" (
				id, "recipientId", kind, "leadId", title, body, read, "createdAt"
			) VALUES `+strings.Join(values, ","), args...)
		if err != nil {
			log.Printf("create notification batch: %v", err)
		}
	}
}

func nullIfEmpty(s string) *string {
	s = strings.TrimSpace(s)
	if s == "" {
		return nil
	}
	return &s
}

func utf8Len(s string) int {
	return len([]rune(s))
}

func (s *NotificationStore) NotifyStaff(
	ctx context.Context,
	actorID, kind, title, body string,
	leadID *string,
	extraRecipients ...string,
) {
	staff, err := s.staffRecipientIDs(ctx, actorID)
	if err != nil {
		log.Printf("notification recipients: %v", err)
		return
	}
	recipients := mergeRecipientIDs(staff, extraRecipients...)
	// Never notify the actor.
	filtered := make([]string, 0, len(recipients))
	for _, id := range recipients {
		if id != strings.TrimSpace(actorID) {
			filtered = append(filtered, id)
		}
	}
	s.CreateMany(ctx, filtered, kind, title, body, leadID)
}

func (s *NotificationStore) ListForUser(ctx context.Context, userID string, limit int) (NotificationListResponse, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return NotificationListResponse{Items: []AppNotification{}}, fmt.Errorf("user required")
	}
	if limit <= 0 || limit > 100 {
		limit = 40
	}

	var unread int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int FROM "Notification"
		WHERE "recipientId" = $1 AND read = false`, userID,
	).Scan(&unread); err != nil {
		return NotificationListResponse{}, err
	}

	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, title, body, read, "createdAt", "leadId"
		FROM "Notification"
		WHERE "recipientId" = $1
		ORDER BY "createdAt" DESC
		LIMIT $2`, userID, limit)
	if err != nil {
		return NotificationListResponse{}, err
	}
	defer rows.Close()

	items := make([]AppNotification, 0, limit)
	for rows.Next() {
		var n AppNotification
		var body *string
		var leadID *string
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &body, &n.Read, &n.CreatedAt, &leadID); err != nil {
			return NotificationListResponse{}, err
		}
		if body != nil {
			n.Body = *body
		}
		n.LeadID = leadID
		searchHint := ""
		if n.Kind == NotifLeadTransfer || n.Kind == NotifSETeamTransfer {
			searchHint = n.Body
			if parts := strings.Split(n.Body, " · "); len(parts) > 0 {
				searchHint = strings.TrimSpace(parts[0])
			}
		}
		n.Href = notificationHref(n.Kind, leadID, searchHint)
		items = append(items, n)
	}
	if err := rows.Err(); err != nil {
		return NotificationListResponse{}, err
	}
	return NotificationListResponse{Items: items, UnreadCount: unread}, nil
}

func (s *NotificationStore) MarkRead(ctx context.Context, userID string, ids []string, all bool) (int, error) {
	userID = strings.TrimSpace(userID)
	if userID == "" {
		return 0, fmt.Errorf("user required")
	}
	if all {
		tag, err := s.pool.Exec(ctx, `
			UPDATE "Notification"
			SET read = true
			WHERE "recipientId" = $1 AND read = false`, userID)
		if err != nil {
			return 0, err
		}
		return int(tag.RowsAffected()), nil
	}
	clean := make([]string, 0, len(ids))
	for _, id := range ids {
		id = strings.TrimSpace(id)
		if id != "" {
			clean = append(clean, id)
		}
	}
	if len(clean) == 0 {
		return 0, nil
	}
	tag, err := s.pool.Exec(ctx, `
		UPDATE "Notification"
		SET read = true
		WHERE "recipientId" = $1 AND id = ANY($2) AND read = false`, userID, clean)
	if err != nil {
		return 0, err
	}
	return int(tag.RowsAffected()), nil
}

func (s *Server) handleNotifications(w http.ResponseWriter, r *http.Request) {
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	switch r.Method {
	case http.MethodGet:
		limit := 40
		if raw := strings.TrimSpace(r.URL.Query().Get("limit")); raw != "" {
			if n, err := strconv.Atoi(raw); err == nil {
				limit = n
			}
		}
		result, err := s.notifications.ListForUser(r.Context(), authUser.ID, limit)
		if err != nil {
			log.Printf("list notifications: %v", err)
			writeError(w, http.StatusInternalServerError, "failed to load notifications")
			return
		}
		writeJSON(w, http.StatusOK, result)
	default:
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
	}
}

type MarkNotificationsReadRequest struct {
	IDs []string `json:"ids"`
	All bool     `json:"all"`
}

func (s *Server) handleNotificationsRead(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodPost {
		writeError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	authUser, ok := userFromContext(r.Context())
	if !ok {
		writeError(w, http.StatusUnauthorized, "unauthorized")
		return
	}
	var req MarkNotificationsReadRequest
	if err := decodeJSON(r, &req); err != nil {
		writeError(w, http.StatusBadRequest, "invalid JSON body")
		return
	}
	n, err := s.notifications.MarkRead(r.Context(), authUser.ID, req.IDs, req.All)
	if err != nil {
		log.Printf("mark notifications read: %v", err)
		writeError(w, http.StatusInternalServerError, "failed to update notifications")
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"updated": n})
}
