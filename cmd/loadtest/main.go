// loadtest hammers LeadFlow like concurrent humans: list/search/paginate leads,
// hit aggregates, open lead details, and create→patch→delete
// disposable leads. Ramps concurrency and prints a production capacity report.
//
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"math"
	"math/rand"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"sync/atomic"
	"time"
)

type loginResp struct {
	Token string `json:"token"`
}

type leadsPage struct {
	Items      []map[string]any `json:"items"`
	NextCursor string           `json:"nextCursor"`
	HasMore    bool             `json:"hasMore"`
	Total      int64            `json:"total"`
}

type sample struct {
	name   string
	dur    time.Duration
	status int
	err    string
}

type metrics struct {
	mu      sync.Mutex
	byOp    map[string]*opStats
	samples []sample
}

type opStats struct {
	ok, fail int64
	durs     []time.Duration
	errs     map[string]int
}

func newMetrics() *metrics {
	return &metrics{byOp: make(map[string]*opStats)}
}

func (m *metrics) record(name string, dur time.Duration, status int, err error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	st := m.byOp[name]
	if st == nil {
		st = &opStats{errs: make(map[string]int)}
		m.byOp[name] = st
	}
	st.durs = append(st.durs, dur)
	msg := ""
	if err != nil {
		st.fail++
		msg = err.Error()
		st.errs[truncate(msg, 80)]++
	} else if status >= 500 || status == 0 {
		st.fail++
		msg = fmt.Sprintf("http_%d", status)
		st.errs[msg]++
	} else if status >= 400 {
		st.fail++
		msg = fmt.Sprintf("http_%d", status)
		st.errs[msg]++
	} else {
		st.ok++
	}
	m.samples = append(m.samples, sample{name: name, dur: dur, status: status, err: msg})
}

func percentile(sorted []time.Duration, p float64) time.Duration {
	if len(sorted) == 0 {
		return 0
	}
	idx := int(math.Ceil(p*float64(len(sorted)))) - 1
	if idx < 0 {
		idx = 0
	}
	if idx >= len(sorted) {
		idx = len(sorted) - 1
	}
	return sorted[idx]
}

func (m *metrics) report(users int, wall time.Duration) {
	m.mu.Lock()
	defer m.mu.Unlock()

	var totalOK, totalFail int64
	var allDurs []time.Duration
	names := make([]string, 0, len(m.byOp))
	for name, st := range m.byOp {
		names = append(names, name)
		totalOK += st.ok
		totalFail += st.fail
		allDurs = append(allDurs, st.durs...)
	}
	sort.Strings(names)
	sort.Slice(allDurs, func(i, j int) bool { return allDurs[i] < allDurs[j] })

	total := totalOK + totalFail
	errRate := 0.0
	if total > 0 {
		errRate = 100 * float64(totalFail) / float64(total)
	}
	rps := float64(total) / wall.Seconds()

	fmt.Printf("\n========== STAGE: %d concurrent users · %s ==========\n", users, wall.Round(time.Millisecond))
	fmt.Printf("requests=%d  ok=%d  fail=%d  error_rate=%.2f%%  throughput=%.1f req/s\n",
		total, totalOK, totalFail, errRate, rps)
	if len(allDurs) > 0 {
		fmt.Printf("latency overall  p50=%s  p95=%s  p99=%s  max=%s\n",
			percentile(allDurs, 0.50).Round(time.Millisecond),
			percentile(allDurs, 0.95).Round(time.Millisecond),
			percentile(allDurs, 0.99).Round(time.Millisecond),
			allDurs[len(allDurs)-1].Round(time.Millisecond),
		)
	}
	fmt.Println("per-operation:")
	for _, name := range names {
		st := m.byOp[name]
		durs := append([]time.Duration(nil), st.durs...)
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		n := st.ok + st.fail
		er := 0.0
		if n > 0 {
			er = 100 * float64(st.fail) / float64(n)
		}
		fmt.Printf("  %-18s n=%5d  err=%.1f%%  p50=%6s  p95=%6s  p99=%6s\n",
			name, n, er,
			percentile(durs, 0.50).Round(time.Millisecond),
			percentile(durs, 0.95).Round(time.Millisecond),
			percentile(durs, 0.99).Round(time.Millisecond),
		)
		if len(st.errs) > 0 {
			type kv struct {
				k string
				v int
			}
			var list []kv
			for k, v := range st.errs {
				list = append(list, kv{k, v})
			}
			sort.Slice(list, func(i, j int) bool { return list[i].v > list[j].v })
			for i, e := range list {
				if i >= 3 {
					break
				}
				fmt.Printf("      · %dx %s\n", e.v, e.k)
			}
		}
	}
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "…"
}

type client struct {
	base   string
	token  string
	http   *http.Client
	rng    *rand.Rand
	met    *metrics
	writes bool
}

func (c *client) do(ctx context.Context, op, method, path string, body any) (int, []byte, time.Duration, error) {
	var rdr io.Reader
	if body != nil {
		b, err := json.Marshal(body)
		if err != nil {
			return 0, nil, 0, err
		}
		rdr = bytes.NewReader(b)
	}
	req, err := http.NewRequestWithContext(ctx, method, c.base+path, rdr)
	if err != nil {
		return 0, nil, 0, err
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	if c.token != "" {
		req.Header.Set("Authorization", "Bearer "+c.token)
	}
	start := time.Now()
	res, err := c.http.Do(req)
	dur := time.Since(start)
	if err != nil {
		c.met.record(op, dur, 0, err)
		return 0, nil, dur, err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(io.LimitReader(res.Body, 4<<20))
	if res.StatusCode >= 400 {
		c.met.record(op, dur, res.StatusCode, fmt.Errorf("%s", strings.TrimSpace(string(data))))
	} else {
		c.met.record(op, dur, res.StatusCode, nil)
	}
	return res.StatusCode, data, dur, nil
}

func (c *client) listPage(ctx context.Context, q string, cursor string) (leadsPage, error) {
	vals := url.Values{}
	vals.Set("filter", "all")
	vals.Set("sort", "newest")
	vals.Set("limit", "40")
	if q != "" {
		vals.Set("q", q)
		vals.Set("field", "lead")
	}
	if cursor != "" {
		vals.Set("cursor", cursor)
	}
	path := "/api/leads?" + vals.Encode()
	status, data, _, err := c.do(ctx, "list_leads", http.MethodGet, path, nil)
	if err != nil || status >= 400 {
		return leadsPage{}, fmt.Errorf("list failed: %d %v", status, err)
	}
	var page leadsPage
	_ = json.Unmarshal(data, &page)
	return page, nil
}

func (c *client) sessionLoop(ctx context.Context) {
	queries := []string{"rahul", "sharma", "amit", "priya", "9876", "india", "a"}
	for {
		if ctx.Err() != nil {
			return
		}
		// Think time: active users don't fire every ms.
		think := time.Duration(80+c.rng.Intn(220)) * time.Millisecond
		select {
		case <-ctx.Done():
			return
		case <-time.After(think):
		}

		roll := c.rng.Intn(100)
		switch {
		case roll < 35:
			// Browse + scroll 1–3 pages
			page, err := c.listPage(ctx, "", "")
			if err != nil || ctx.Err() != nil {
				continue
			}
			pages := 1 + c.rng.Intn(3)
			for i := 0; i < pages && page.HasMore && page.NextCursor != ""; i++ {
				page, err = c.listPage(ctx, "", page.NextCursor)
				if err != nil || ctx.Err() != nil {
					break
				}
			}
		case roll < 50:
			q := queries[c.rng.Intn(len(queries))]
			_, _ = c.listPage(ctx, q, "")
		case roll < 62:
			_, _, _, _ = c.do(ctx, "summary", http.MethodGet, "/api/leads/summary", nil)
		case roll < 72:
			_, _, _, _ = c.do(ctx, "pipeline_sum", http.MethodGet, "/api/leads/pipeline/summary", nil)
		case roll < 80:
			_, _, _, _ = c.do(ctx, "geo_options", http.MethodGet, "/api/leads/geo-options", nil)
		case roll < 88:
			// Open a lead from a fresh list page.
			page, err := c.listPage(ctx, "", "")
			if err != nil || len(page.Items) == 0 {
				continue
			}
			id, _ := page.Items[c.rng.Intn(len(page.Items))]["id"].(string)
			if id == "" {
				continue
			}
			_, _, _, _ = c.do(ctx, "lead_detail", http.MethodGet, "/api/leads/"+id, nil)
		case roll < 94:
			_, _, _, _ = c.do(ctx, "notifications", http.MethodGet, "/api/notifications", nil)
		default:
			if !c.writes {
				_, _ = c.listPage(ctx, "", "")
				continue
			}
			c.writeCycle(ctx)
		}
	}
}

func (c *client) writeCycle(ctx context.Context) {
	suffix := fmt.Sprintf("%d-%d", time.Now().UnixNano(), c.rng.Intn(1_000_000))
	phone := fmt.Sprintf("+9100%08d", c.rng.Int63n(99_999_999))
	email := fmt.Sprintf("loadtest+%s@example.invalid", suffix)
	name := "[loadtest] Worker " + suffix
	body := map[string]any{
		"fullName":            name,
		"email":               email,
		"phone":               phone,
		"country":             "India",
		"city":                "LoadTestCity",
		"source":              "Website WhatsApp",
		"qualificationStatus": "NOT_QUALIFIED",
		"notes":               "loadtest create",
	}
	status, data, _, err := c.do(ctx, "lead_create", http.MethodPost, "/api/leads", body)
	if err != nil || status >= 400 {
		return
	}
	var created struct {
		ID string `json:"id"`
	}
	_ = json.Unmarshal(data, &created)
	if created.ID == "" {
		return
	}
	// Detail fetch — realistic "open lead" read path.
	_, _, _, _ = c.do(ctx, "lead_detail", http.MethodGet, "/api/leads/"+created.ID, nil)
	// Full profile patch (API requires the create-shaped body).
	_, _, _, _ = c.do(ctx, "lead_patch", http.MethodPatch, "/api/leads/"+created.ID, map[string]any{
		"fullName":            name + " edited",
		"email":               email,
		"phone":               phone,
		"country":             "India",
		"city":                "LoadTestCity",
		"source":              "Website WhatsApp",
		"qualificationStatus": "NOT_QUALIFIED",
		"notes":               "loadtest patch " + suffix,
	})
	_, _, _, _ = c.do(ctx, "lead_delete", http.MethodDelete, "/api/leads", map[string]any{
		"leadIds": []string{created.ID},
	})
}

func login(base, email, password string, httpClient *http.Client) (string, error) {
	payload, _ := json.Marshal(map[string]string{"email": email, "password": password})
	req, err := http.NewRequest(http.MethodPost, base+"/api/auth/login", bytes.NewReader(payload))
	if err != nil {
		return "", err
	}
	req.Header.Set("Content-Type", "application/json")
	res, err := httpClient.Do(req)
	if err != nil {
		return "", err
	}
	defer res.Body.Close()
	data, _ := io.ReadAll(res.Body)
	if res.StatusCode >= 400 {
		return "", fmt.Errorf("login %d: %s", res.StatusCode, data)
	}
	var out loginResp
	if err := json.Unmarshal(data, &out); err != nil {
		return "", err
	}
	if out.Token == "" {
		return "", fmt.Errorf("empty token")
	}
	return out.Token, nil
}

func runStage(base, token string, users int, duration time.Duration, writes bool, httpClient *http.Client) *metrics {
	met := newMetrics()
	ctx, cancel := context.WithTimeout(context.Background(), duration)
	defer cancel()

	var wg sync.WaitGroup
	var started atomic.Int64
	for i := 0; i < users; i++ {
		wg.Add(1)
		go func(id int) {
			defer wg.Done()
			c := &client{
				base:   base,
				token:  token,
				http:   httpClient,
				rng:    rand.New(rand.NewSource(time.Now().UnixNano() + int64(id)*9973)),
				met:    met,
				writes: writes,
			}
			started.Add(1)
			c.sessionLoop(ctx)
		}(i)
	}
	// Wait until stage window ends, then cancel; workers exit on ctx.
	<-ctx.Done()
	wg.Wait()
	met.report(users, duration)
	return met
}

func stageHealthy(m *metrics) (ok bool, reason string) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var totalOK, totalFail int64
	var all []time.Duration
	for _, st := range m.byOp {
		totalOK += st.ok
		totalFail += st.fail
		all = append(all, st.durs...)
	}
	total := totalOK + totalFail
	if total == 0 {
		return false, "no requests completed"
	}
	errRate := float64(totalFail) / float64(total)
	sort.Slice(all, func(i, j int) bool { return all[i] < all[j] })
	p95 := percentile(all, 0.95)
	if errRate > 0.05 {
		return false, fmt.Sprintf("error rate %.1f%% > 5%%", errRate*100)
	}
	if p95 > 3*time.Second {
		return false, fmt.Sprintf("p95 latency %s > 3s", p95.Round(time.Millisecond))
	}
	return true, "within SLO (err≤5%, p95≤3s)"
}

func main() {
	base := flag.String("base", envOr("LOADTEST_BASE", "http://127.0.0.1:8080"), "API base URL")
	email := flag.String("email", envOr("LOADTEST_EMAIL", "superadmin@demo.local"), "login email")
	password := flag.String("password", envOr("LOADTEST_PASSWORD", "LeadFlow1!"), "login password")
	duration := flag.Duration("duration", 45*time.Second, "duration per concurrency stage")
	writes := flag.Bool("writes", true, "include create/patch/delete lead cycles (~6% of actions)")
	stagesFlag := flag.String("stages", "25,50,75,100,150,200", "comma-separated concurrency stages")
	flag.Parse()

	httpClient := &http.Client{
		Timeout: 20 * time.Second,
		Transport: &http.Transport{
			MaxIdleConns:        512,
			MaxIdleConnsPerHost: 512,
			MaxConnsPerHost:     0,
			IdleConnTimeout:     90 * time.Second,
			ForceAttemptHTTP2:   true,
		},
	}

	fmt.Println("LeadFlow production load test")
	fmt.Printf("target=%s  writes=%v  stage_duration=%s\n", *base, *writes, *duration)

	healthCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	req, _ := http.NewRequestWithContext(healthCtx, http.MethodGet, *base+"/health", nil)
	res, err := httpClient.Do(req)
	cancel()
	if err != nil || res.StatusCode != 200 {
		fmt.Fprintf(os.Stderr, "backend not healthy: %v\n", err)
		os.Exit(1)
	}
	res.Body.Close()
	fmt.Println("health: ok")

	token, err := login(*base, *email, *password, httpClient)
	if err != nil {
		fmt.Fprintf(os.Stderr, "login failed: %v\n", err)
		os.Exit(1)
	}
	fmt.Printf("auth: ok (token len=%d)\n", len(token))

	var stages []int
	for _, part := range strings.Split(*stagesFlag, ",") {
		part = strings.TrimSpace(part)
		if part == "" {
			continue
		}
		var n int
		if _, err := fmt.Sscanf(part, "%d", &n); err == nil && n > 0 {
			stages = append(stages, n)
		}
	}
	if len(stages) == 0 {
		fmt.Fprintln(os.Stderr, "no stages")
		os.Exit(1)
	}

	type result struct {
		users   int
		ok      bool
		reason  string
		errRate float64
		p95     time.Duration
		rps     float64
	}
	var results []result
	breakAt := 0

	for _, n := range stages {
		fmt.Printf("\n>>> warming %d users…\n", n)
		met := runStage(*base, token, n, *duration, *writes, httpClient)
		ok, reason := stageHealthy(met)

		met.mu.Lock()
		var okN, failN int64
		var durs []time.Duration
		for _, st := range met.byOp {
			okN += st.ok
			failN += st.fail
			durs = append(durs, st.durs...)
		}
		met.mu.Unlock()
		total := okN + failN
		er := 0.0
		if total > 0 {
			er = float64(failN) / float64(total)
		}
		sort.Slice(durs, func(i, j int) bool { return durs[i] < durs[j] })
		p95 := percentile(durs, 0.95)
		rps := float64(total) / duration.Seconds()
		results = append(results, result{users: n, ok: ok, reason: reason, errRate: er, p95: p95, rps: rps})
		fmt.Printf(">>> verdict @ %d users: %s (%s)\n", n, map[bool]string{true: "PASS", false: "FAIL"}[ok], reason)
		if !ok {
			breakAt = n
			fmt.Println(">>> stopping ramp — SLO breached (capacity found)")
			break
		}
	}

	fmt.Println("\n==================== CAPACITY REPORT ====================")
	fmt.Println("SLO: error rate ≤ 5% AND overall p95 latency ≤ 3s")
	fmt.Println("Workload mix: browse+paginate ~35%, search ~15%, summary ~12%,")
	fmt.Println("  pipeline ~10%, geo ~8%, open lead ~8%,")
	fmt.Println("  notifications ~6%, create/patch/delete lead ~6%")
	fmt.Println()
	maxPass := 0
	for _, r := range results {
		mark := "PASS"
		if !r.ok {
			mark = "FAIL"
		}
		fmt.Printf("  %3d users  %-4s  err=%.2f%%  p95=%s  ≈%.0f req/s  (%s)\n",
			r.users, mark, r.errRate*100, r.p95.Round(time.Millisecond), r.rps, r.reason)
		if r.ok {
			maxPass = r.users
		}
	}
	fmt.Println()
	if maxPass > 0 {
		fmt.Printf("RECOMMENDED safe concurrent active users: %d\n", maxPass)
		fmt.Printf("(Peak tested without SLO breach. Headroom: stay ≤ ~80%% of this in production.)\n")
		fmt.Printf("Production planning number: ~%d concurrent active users\n", int(float64(maxPass)*0.8))
	} else {
		fmt.Println("RECOMMENDED safe concurrent active users: < first stage (SLO failed immediately)")
	}
	if breakAt > 0 {
		fmt.Printf("BREAKING POINT observed at: %d concurrent users\n", breakAt)
	} else {
		fmt.Println("BREAKING POINT: not reached within staged ramp")
	}
	fmt.Println("=========================================================")
}

func envOr(key, fallback string) string {
	if v := strings.TrimSpace(os.Getenv(key)); v != "" {
		return v
	}
	return fallback
}
