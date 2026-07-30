package main

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

type Overview struct {
	LeadsTotal     int          `json:"leadsTotal"`
	LeadsLast7Days int          `json:"leadsLast7Days"`
	UsersTotal     int          `json:"usersTotal"`
	TeamsTotal     int          `json:"teamsTotal"`
	Notifications  int          `json:"notifications"`
	UnreadNotifs   int          `json:"unreadNotifications"`
	HandoffsTotal  int          `json:"handoffsTotal"`
	Qualification  []NamedCount `json:"qualification"`
	SalesStages    []NamedCount `json:"salesStages"`
	TopSources     []NamedCount `json:"topSources"`
	Roles          []NamedCount `json:"roles"`
}

type NamedCount struct {
	Name  string `json:"name"`
	Count int    `json:"count"`
}

type Lead struct {
	ID                  string    `json:"id"`
	LeadName            string    `json:"leadName"`
	Phone               *string   `json:"phone"`
	LeadEmail           *string   `json:"leadEmail"`
	Country             *string   `json:"country"`
	City                *string   `json:"city"`
	Source              string    `json:"source"`
	QualificationStatus string    `json:"qualificationStatus"`
	LeadScore           *int      `json:"leadScore"`
	SalesStage          string    `json:"salesStage"`
	PortalWebsite       *string   `json:"portalWebsite"`
	DealCurrency        string    `json:"dealCurrency"`
	CreatedAt           time.Time `json:"createdAt"`
	UpdatedAt           time.Time `json:"updatedAt"`
}

type Team struct {
	ID        string    `json:"id"`
	Name      string    `json:"name"`
	CreatedAt time.Time `json:"createdAt"`
	LeadCount int       `json:"leadCount"`
	UserCount int       `json:"userCount"`
}

type Notification struct {
	ID        string    `json:"id"`
	Kind      string    `json:"kind"`
	Title     string    `json:"title"`
	Body      *string   `json:"body"`
	Read      bool      `json:"read"`
	CreatedAt time.Time `json:"createdAt"`
}

type DashboardResponse struct {
	Overview      Overview       `json:"overview"`
	RecentLeads   []Lead         `json:"recentLeads"`
	Teams         []Team         `json:"teams"`
	Users         []PublicUser   `json:"users"`
	Notifications []Notification `json:"notifications"`
}

type DashboardStore struct {
	pool *pgxpool.Pool
}

func NewDashboardStore(pool *pgxpool.Pool) *DashboardStore {
	return &DashboardStore{pool: pool}
}

func namedCounts(ctx context.Context, pool *pgxpool.Pool, query string) ([]NamedCount, error) {
	return namedCountsArgs(ctx, pool, query)
}

func namedCountsArgs(ctx context.Context, pool *pgxpool.Pool, query string, args ...any) ([]NamedCount, error) {
	rows, err := pool.Query(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []NamedCount
	for rows.Next() {
		var item NamedCount
		if err := rows.Scan(&item.Name, &item.Count); err != nil {
			return nil, err
		}
		out = append(out, item)
	}
	return out, rows.Err()
}

func (s *DashboardStore) Overview(ctx context.Context) (Overview, error) {
	var o Overview
	queries := []struct {
		dest *int
		sql  string
	}{
		{&o.LeadsTotal, `SELECT COUNT(*) FROM "Lead"`},
		{&o.LeadsLast7Days, `SELECT COUNT(*) FROM "Lead" WHERE "createdAt" > NOW() - INTERVAL '7 days'`},
		{&o.UsersTotal, `SELECT COUNT(*) FROM "User"`},
		{&o.TeamsTotal, `SELECT COUNT(*) FROM "Team"`},
		{&o.Notifications, `SELECT COUNT(*) FROM "Notification"`},
		{&o.UnreadNotifs, `SELECT COUNT(*) FROM "Notification" WHERE read = false`},
		{&o.HandoffsTotal, `SELECT COUNT(*) FROM "LeadHandoffLog"`},
	}
	for _, q := range queries {
		if err := s.pool.QueryRow(ctx, q.sql).Scan(q.dest); err != nil {
			return o, err
		}
	}

	var err error
	o.Qualification, err = namedCounts(ctx, s.pool, `
		SELECT "qualificationStatus", COUNT(*)::int
		FROM "Lead" GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		return o, err
	}
	o.SalesStages, err = namedCounts(ctx, s.pool, `
		SELECT "salesStage", COUNT(*)::int
		FROM "Lead" GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		return o, err
	}
	o.TopSources, err = namedCounts(ctx, s.pool, `
		SELECT COALESCE("source", 'Unknown'), COUNT(*)::int
		FROM "Lead" GROUP BY 1 ORDER BY 2 DESC LIMIT 8`)
	if err != nil {
		return o, err
	}
	o.Roles, err = namedCounts(ctx, s.pool, `
		SELECT role, COUNT(*)::int
		FROM "User" GROUP BY 1 ORDER BY 2 DESC`)
	if err != nil {
		return o, err
	}
	return o, nil
}

func (s *DashboardStore) RecentLeads(ctx context.Context, limit int) ([]Lead, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, "leadName", phone, "leadEmail", country, city, source,
		       "qualificationStatus", "leadScore", "salesStage", "portalWebsite",
		       "dealCurrency", "createdAt", "updatedAt"
		FROM "Lead"
		ORDER BY "updatedAt" DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Lead, 0, limit)
	for rows.Next() {
		var l Lead
		if err := rows.Scan(
			&l.ID, &l.LeadName, &l.Phone, &l.LeadEmail, &l.Country, &l.City, &l.Source,
			&l.QualificationStatus, &l.LeadScore, &l.SalesStage, &l.PortalWebsite,
			&l.DealCurrency, &l.CreatedAt, &l.UpdatedAt,
		); err != nil {
			return nil, err
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

func (s *DashboardStore) Teams(ctx context.Context) ([]Team, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT t.id, t.name, t."createdAt",
		       COALESCE((SELECT COUNT(*) FROM "Lead" l WHERE l."teamId" = t.id), 0)::int AS lead_count,
		       COALESCE((SELECT COUNT(*) FROM "User" u WHERE u."teamId" = t.id), 0)::int AS user_count
		FROM "Team" t
		ORDER BY t.name ASC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []Team
	for rows.Next() {
		var t Team
		if err := rows.Scan(&t.ID, &t.Name, &t.CreatedAt, &t.LeadCount, &t.UserCount); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

func (s *DashboardStore) Notifications(ctx context.Context, limit int) ([]Notification, error) {
	rows, err := s.pool.Query(ctx, `
		SELECT id, kind, title, body, read, "createdAt"
		FROM "Notification"
		ORDER BY "createdAt" DESC
		LIMIT $1`, limit)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	out := make([]Notification, 0, limit)
	for rows.Next() {
		var n Notification
		if err := rows.Scan(&n.ID, &n.Kind, &n.Title, &n.Body, &n.Read, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}
