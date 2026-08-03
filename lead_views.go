package main

import (
	"context"
	"fmt"
	"strings"
	"time"
)

func (s *LeadStore) MarkLeadViewed(ctx context.Context, userID, leadID string) error {
	userID = strings.TrimSpace(userID)
	leadID = strings.TrimSpace(leadID)
	if userID == "" || leadID == "" {
		return fmt.Errorf("user and lead are required")
	}
	_, err := s.pool.Exec(ctx, `
		INSERT INTO "LeadView" ("userId", "leadId", "viewedAt")
		VALUES ($1, $2, $3)
		ON CONFLICT ("userId", "leadId")
		DO UPDATE SET "viewedAt" = EXCLUDED."viewedAt"`,
		userID, leadID, time.Now().UTC(),
	)
	return err
}
