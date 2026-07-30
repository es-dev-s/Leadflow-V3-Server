package main

import (
	"bufio"
	"context"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"
)

func testEnvFromFile(t *testing.T, path, key string) string {
	t.Helper()
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer f.Close()
	sc := bufio.NewScanner(f)
	for sc.Scan() {
		line := strings.TrimSpace(sc.Text())
		if strings.HasPrefix(line, "#") || line == "" {
			continue
		}
		if v, ok := strings.CutPrefix(line, key+"="); ok {
			return strings.TrimSpace(v)
		}
	}
	return ""
}

func openTestLeadStore(t *testing.T) *LeadStore {
	t.Helper()
	dbURL := testEnvFromFile(t, ".env", "DATABASE_URL")
	if dbURL == "" {
		t.Skip("DATABASE_URL not configured; skipping DB pagination test")
	}
	cfg, err := pgxpool.ParseConfig(dbURL)
	if err != nil {
		t.Skipf("bad DATABASE_URL: %v", err)
	}
	cfg.MaxConns = 4
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	pool, err := pgxpool.NewWithConfig(ctx, cfg)
	if err != nil {
		t.Skipf("db unreachable: %v", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		t.Skipf("db unreachable: %v", err)
	}
	t.Cleanup(pool.Close)
	return NewLeadStore(pool)
}

// paginate walks the cursor for up to maxPages and asserts every page makes
// forward progress with no duplicate rows — the invariant the UI's infinite
// scroll depends on (the old client-side 500-row cap broke deep scrolls).
func paginate(t *testing.T, store *LeadStore, params LeadListParams, maxPages int) (seenRows int, total int64) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Second)
	defer cancel()

	seen := make(map[string]struct{})
	cursor := ""
	for page := 0; page < maxPages; page++ {
		params.Cursor = cursor
		resp, err := store.List(ctx, params)
		if err != nil {
			t.Fatalf("page %d: %v", page, err)
		}
		if page == 0 {
			total = resp.Total
		}
		if len(resp.Items) == 0 {
			if resp.HasMore {
				t.Fatalf("page %d: empty page but hasMore=true (stalled cursor)", page)
			}
			break
		}
		for _, item := range resp.Items {
			if _, dup := seen[item.ID]; dup {
				t.Fatalf("page %d: duplicate lead id %s across pages", page, item.ID)
			}
			seen[item.ID] = struct{}{}
		}
		if !resp.HasMore {
			break
		}
		if resp.NextCursor == "" {
			t.Fatalf("page %d: hasMore=true but empty nextCursor", page)
		}
		if resp.NextCursor == cursor {
			t.Fatalf("page %d: cursor did not advance", page)
		}
		cursor = resp.NextCursor
	}
	return len(seen), total
}

func TestLeadListPaginatesPast500(t *testing.T) {
	store := openTestLeadStore(t)
	params := LeadListParams{Filter: "all", Sort: "newest", Limit: 40}
	rows, total := paginate(t, store, params, 20) // up to 800 rows
	if total > 800 && rows < 800 {
		t.Fatalf("expected 800 rows over 20 pages, got %d (total=%d)", rows, total)
	}
	t.Logf("unfiltered: walked %d rows, total=%d", rows, total)
}

func TestLeadListFilteredPaginationWholeDB(t *testing.T) {
	store := openTestLeadStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()

	// Most common country: a facet that matches a large slice of the table.
	var country string
	err := store.pool.QueryRow(ctx, `
		SELECT BTRIM(country) FROM "Lead"
		WHERE country IS NOT NULL AND BTRIM(country) <> ''
		GROUP BY 1 ORDER BY COUNT(*) DESC LIMIT 1`).Scan(&country)
	if err != nil {
		t.Skipf("no country data: %v", err)
	}

	var exact int64
	if err := store.pool.QueryRow(ctx, `
		SELECT COUNT(*) FROM "Lead" WHERE BTRIM(country) = $1`, country).Scan(&exact); err != nil {
		t.Fatal(err)
	}

	params := LeadListParams{Filter: "all", Sort: "newest", Limit: 40, Country: country}
	rows, total := paginate(t, store, params, 16) // up to 640 rows
	if total != exact {
		t.Fatalf("filtered total mismatch: API total=%d, direct COUNT=%d (filter must cover whole DB)", total, exact)
	}
	want := int64(16 * 40)
	if exact > want && int64(rows) < want {
		t.Fatalf("filtered walk stalled: got %d rows, expected %d (matching=%d)", rows, want, exact)
	}
	t.Logf("country=%q: walked %d rows, total=%d (matches direct count)", country, rows, total)
}

// TestLeadListAllFieldsSearch exercises the global search box path (no field
// scope), which previously timed out with an 18-column OR and returned 500.
func TestLeadListAllFieldsSearch(t *testing.T) {
	store := openTestLeadStore(t)

	// A name-ish token, a phone fragment, and a rare-ish token: all must
	// come back well under the 8s statement timeout.
	for _, q := range []string{"rahul", "98765", "sharma"} {
		params := LeadListParams{Filter: "all", Sort: "newest", Limit: 40, Query: q}
		start := time.Now()
		rows, total := paginate(t, store, params, 3)
		elapsed := time.Since(start)
		if elapsed > 5*time.Second {
			t.Fatalf("all-fields search %q too slow: %s for 3 pages", q, elapsed)
		}
		t.Logf("all-fields %q: %d rows over 3 pages, total=%d in %s", q, rows, total, elapsed)
	}
}

func TestRedisResponseCacheRoundTrip(t *testing.T) {
	url := testEnvFromFile(t, ".env", "REDIS_URL")
	if url == "" {
		t.Skip("REDIS_URL not configured")
	}
	cache, err := NewRedisResponseCache(url, NewResponseCache())
	if err != nil {
		t.Fatal(err)
	}
	key := "test:" + time.Now().Format(time.RFC3339Nano)
	cache.Set(key, []byte(`{"ok":true}`), 30*time.Second)
	got, ok := cache.Get(key)
	if !ok || string(got) != `{"ok":true}` {
		t.Fatalf("expected cache hit, got ok=%v data=%q", ok, got)
	}
	cache.Clear()
	if _, ok := cache.Get(key); ok {
		t.Fatal("expected miss after Clear (generation bump)")
	}
}

func TestLeadListRecentSortPagination(t *testing.T) {
	store := openTestLeadStore(t)
	params := LeadListParams{Filter: "all", Sort: "recent", Limit: 40}
	rows, total := paginate(t, store, params, 15) // up to 600 rows
	if total > 600 && rows < 600 {
		t.Fatalf("recent sort stalled: got %d rows (total=%d)", rows, total)
	}
	t.Logf("recent: walked %d rows, total=%d", rows, total)
}

func TestLeadListSearchWithFacetPagination(t *testing.T) {
	store := openTestLeadStore(t)
	params := LeadListParams{
		Filter: "all",
		Sort:   "newest",
		Limit:  40,
		Query:  "ra", // broad contains-search to exercise search+cursor together
		Field:  "lead",
	}
	rows, total := paginate(t, store, params, 15)
	if total > 600 && rows < 600 {
		t.Fatalf("search pagination stalled: got %d rows (total=%d)", rows, total)
	}
	t.Logf("search 'ra': walked %d rows, total=%d", rows, total)
}
