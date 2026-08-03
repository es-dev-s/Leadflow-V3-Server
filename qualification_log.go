package main

import (
	"context"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
	"github.com/jackc/pgx/v5"
)

// QualificationStatusChange is one step in a lead's qualification timeline.
type QualificationStatusChange struct {
	FromStatus *string `json:"fromStatus"`
	ToStatus   string  `json:"toStatus"`
	FromLabel  *string `json:"fromLabel"`
	ToLabel    string  `json:"toLabel"`
	ChangedAt  string  `json:"changedAt"`
	ActorName  *string `json:"actorName"`
	Reason     *string `json:"reason"`
	Source     string  `json:"source"`
	// Minutes spent in fromStatus before this change (null for first/create).
	MinutesInPrevious *int `json:"minutesInPrevious"`
	// Minutes spent in toStatus until the next change (or until now if current).
	MinutesInStatus int  `json:"minutesInStatus"`
	IsCurrent       bool `json:"isCurrent"`
}

func insertQualificationLog(
	ctx context.Context,
	tx pgx.Tx,
	leadID, fromStatus, toStatus, actorID, reason, source string,
	at time.Time,
) error {
	leadID = strings.TrimSpace(leadID)
	toStatus = strings.TrimSpace(toStatus)
	if leadID == "" || toStatus == "" {
		return nil
	}
	from := strings.TrimSpace(fromStatus)
	if from != "" && from == toStatus {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	var fromArg any
	if from != "" {
		fromArg = from
	}
	var actorArg any
	if id := strings.TrimSpace(actorID); id != "" {
		actorArg = id
	}
	var reasonArg any
	if r := strings.TrimSpace(reason); r != "" {
		reasonArg = r
	}
	src := strings.TrimSpace(source)
	if src == "" {
		src = "update"
	}

	_, err := tx.Exec(ctx, `
		INSERT INTO "LeadQualificationStatusLog" (
			id, "leadId", "fromStatus", "toStatus", "changedAt",
			"actorId", reason, source
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		uuid.NewString(), leadID, fromArg, toStatus, at.UTC(),
		actorArg, reasonArg, src,
	)
	return err
}

// recordQualificationChange writes a history row and stamps qualificationEnteredAt.
func (s *LeadStore) recordQualificationChange(
	ctx context.Context,
	leadID, fromStatus, toStatus, actorID, reason, source string,
	at time.Time,
) error {
	leadID = strings.TrimSpace(leadID)
	toStatus = strings.TrimSpace(toStatus)
	from := strings.TrimSpace(fromStatus)
	if leadID == "" || toStatus == "" {
		return nil
	}
	if from != "" && from == toStatus {
		return nil
	}
	if at.IsZero() {
		at = time.Now().UTC()
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return err
	}
	defer tx.Rollback(ctx)

	if err := insertQualificationLog(ctx, tx, leadID, from, toStatus, actorID, reason, source, at); err != nil {
		return err
	}
	if _, err := tx.Exec(ctx, `
		UPDATE "Lead"
		SET "qualificationEnteredAt" = $2
		WHERE id = $1`, leadID, at.UTC()); err != nil {
		return err
	}
	return tx.Commit(ctx)
}

func (s *LeadStore) ListQualificationHistory(
	ctx context.Context,
	leadID string,
) ([]QualificationStatusChange, error) {
	leadID = strings.TrimSpace(leadID)
	if leadID == "" {
		return nil, nil
	}

	rows, err := s.pool.Query(ctx, `
		SELECT
			l."fromStatus",
			l."toStatus",
			l."changedAt",
			NULLIF(BTRIM(COALESCE(u.name, '')), ''),
			l.reason,
			l.source
		FROM "LeadQualificationStatusLog" l
		LEFT JOIN "User" u ON u.id = l."actorId"
		WHERE l."leadId" = $1
		ORDER BY l."changedAt" ASC, l.id ASC`, leadID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	type raw struct {
		from   *string
		to     string
		at     time.Time
		actor  *string
		reason *string
		source string
	}
	raws := make([]raw, 0, 16)
	for rows.Next() {
		var r raw
		if err := rows.Scan(&r.from, &r.to, &r.at, &r.actor, &r.reason, &r.source); err != nil {
			return nil, err
		}
		raws = append(raws, r)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	out := make([]QualificationStatusChange, 0, len(raws))
	for i, r := range raws {
		item := QualificationStatusChange{
			FromStatus: r.from,
			ToStatus:   r.to,
			ToLabel:    qualificationDisplay(r.to),
			ChangedAt:  r.at.UTC().Format(time.RFC3339),
			ActorName:  r.actor,
			Reason:     r.reason,
			Source:     r.source,
			IsCurrent:  i == len(raws)-1,
		}
		if r.from != nil && strings.TrimSpace(*r.from) != "" {
			label := qualificationDisplay(*r.from)
			item.FromLabel = &label
		}
		if i > 0 {
			mins := int(math.Round(r.at.Sub(raws[i-1].at).Minutes()))
			if mins < 0 {
				mins = 0
			}
			item.MinutesInPrevious = &mins
		}
		end := now
		if i+1 < len(raws) {
			end = raws[i+1].at
		}
		minsIn := int(math.Round(end.Sub(r.at).Minutes()))
		if minsIn < 0 {
			minsIn = 0
		}
		item.MinutesInStatus = minsIn
		out = append(out, item)
	}
	return out, nil
}

// ensureQualificationLogSeeded backfills a create row for older leads so
// "time in current status" still has a start point.
func (s *LeadStore) ensureQualificationLogSeeded(
	ctx context.Context,
	leadID, status string,
	createdAt time.Time,
) error {
	leadID = strings.TrimSpace(leadID)
	status = strings.TrimSpace(status)
	if leadID == "" || status == "" {
		return nil
	}
	var n int
	if err := s.pool.QueryRow(ctx, `
		SELECT COUNT(*)::int FROM "LeadQualificationStatusLog" WHERE "leadId" = $1`,
		leadID).Scan(&n); err != nil {
		return err
	}
	if n > 0 {
		return nil
	}
	at := createdAt
	if at.IsZero() {
		at = time.Now().UTC()
	}
	return s.recordQualificationChange(ctx, leadID, "", status, "", "", "backfill", at)
}
