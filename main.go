package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgxpool"
)

func main() {
	loadEnvFile(".env")
	corsConfig = loadCORSConfig()

	databaseURL := envOr("DATABASE_URL", "")
	if databaseURL == "" {
		log.Fatal("DATABASE_URL is required in backend/.env")
	}

	jwtSecret := envOr("JWT_SECRET", "")
	if jwtSecret == "" {
		log.Fatal("JWT_SECRET is required in backend/.env")
	}
	tokenTTL := 24 * time.Hour
	authCookie := loadAuthCookieConfig(tokenTTL)

	poolConfig, err := pgxpool.ParseConfig(databaseURL)
	if err != nil {
		log.Fatalf("invalid DATABASE_URL: %v", err)
	}
	// Keep startup fast when the DB host is unreachable.
	poolConfig.ConnConfig.ConnectTimeout = 3 * time.Second
	// Sized for ~200 concurrent browser sessions (most idle; active queries share the pool).
	poolConfig.MaxConns = 96
	poolConfig.MinConns = 8
	poolConfig.MaxConnLifetime = 30 * time.Minute
	poolConfig.MaxConnIdleTime = 5 * time.Minute
	poolConfig.HealthCheckPeriod = 30 * time.Second
	poolConfig.AfterConnect = func(ctx context.Context, conn *pgx.Conn) error {
		_, err := conn.Exec(ctx, `SET statement_timeout = '8s'`)
		return err
	}

	ctx := context.Background()
	pool, err := pgxpool.NewWithConfig(ctx, poolConfig)
	if err != nil {
		log.Fatalf("unable to create database pool: %v", err)
	}
	defer pool.Close()

	pingCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := pool.Ping(pingCtx); err != nil {
		log.Printf("database unavailable at startup: %v (API still serving; /health may report degraded)", err)
	} else {
		log.Println("Connected to PostgreSQL")
		migrateCtx, migrateCancel := context.WithTimeout(ctx, 2*time.Minute)
		if err := RunCRMMigrations(migrateCtx, pool); err != nil {
			log.Printf("migrations failed: %v", err)
		}
		migrateCancel()
		seedCtx, seedCancel := context.WithTimeout(ctx, 15*time.Second)
		if err := NewLeadStore(pool).SeedKpiTargets(seedCtx); err != nil {
			log.Printf("seed KpiTarget rows: %v", err)
		}
		seedCancel()
	}

	memCache := NewResponseCache()
	var respCache ResponseCacher = memCache
	if redisURL := envOr("REDIS_URL", ""); redisURL != "" {
		if rc, err := NewRedisResponseCache(redisURL, memCache); err != nil {
			log.Printf("redis cache disabled (bad REDIS_URL): %v", err)
		} else {
			respCache = rc
			log.Println("Redis response cache enabled")
		}
	}

	uploadStore, err := NewUploadStore(envOr("UPLOAD_DIR", "uploads"))
	if err != nil {
		log.Fatalf("upload storage: %v", err)
	}

	server := &Server{
		users:         NewUserStore(pool),
		dashboard:     NewDashboardStore(pool),
		leads:         NewLeadStore(pool),
		transfers:     NewTransferStore(pool),
		notifications: NewNotificationStore(pool),
		tokens:        NewTokenService(jwtSecret, tokenTTL),
		authCookie:    authCookie,
		loginGate:     newLoginLimiter(12, 10*time.Minute),
		poolPing: func(c context.Context) error {
			pingCtx, cancel := context.WithTimeout(c, 2*time.Second)
			defer cancel()
			return pool.Ping(pingCtx)
		},
		hub:       NewRealtimeHub(),
		respCache: respCache,
		telemetry: NewTelemetryEmitter(
			envOr("TELEMETRY_URL", ""),
			envOr("TELEMETRY_INGEST_SECRET", jwtSecret),
		),
		uploads: uploadStore,
	}

	mux := http.NewServeMux()
	// Public
	mux.HandleFunc("/health", withCORS(server.handleHealth))
	mux.HandleFunc("/api/health", withCORS(server.handleHealth))
	mux.HandleFunc("/api/auth/login", withCORS(server.handleLogin))
	mux.HandleFunc("/api/auth/logout", withCORS(server.handleLogout))

	// Authenticated — cookie (browser) or Authorization Bearer (API clients).
	mux.HandleFunc("/api/auth/me", server.requireAuth(server.handleMe))
	mux.HandleFunc("/api/roles", server.requireAuth(server.handleRoles))
	mux.HandleFunc("/api/users", server.requireAuth(server.handleUsers))
	mux.HandleFunc("/api/users/", server.requireAuth(server.handleUserByID))
	mux.HandleFunc("/api/teams", server.requireAuth(server.handleTeams))
	mux.HandleFunc("/api/leads", server.requireAuth(server.handleLeads))
	mux.HandleFunc("/api/leads/contact-lookup", server.requireAuth(server.handleLeadContactLookup))
	mux.HandleFunc("/api/leads/assign", server.requireAuth(server.handleAssignLeads))
	mux.HandleFunc("/api/leads/summary", server.requireAuth(server.handleLeadsSummary))
	mux.HandleFunc("/api/leads/summary/buckets", server.requireAuth(server.handleLeadsSummaryBuckets))
	mux.HandleFunc("/api/leads/pipeline/summary", server.requireAuth(server.handleLeadsPipelineSummary))
	mux.HandleFunc("/api/kpi", server.requireAuth(server.handleKPI))
	mux.HandleFunc("/api/kpi/targets", server.requireAuth(server.handleKPITargets))
	mux.HandleFunc("/api/uploads/first-response-proof", server.requireAuth(server.handleUploadFirstResponseProof))
	mux.HandleFunc("/api/uploads/first-response/", server.requireAuth(server.handleServeFirstResponseProof))
	mux.HandleFunc("/api/leads/geography", server.requireAuth(server.handleLeadsGeography))
	mux.HandleFunc("/api/leads/geo-options", server.requireAuth(server.handleLeadsGeoOptions))
	mux.HandleFunc("/api/leads/added-series", server.requireAuth(server.handleLeadsAddedSeries))
	mux.HandleFunc("/api/leads/", server.requireAuth(server.handleLeadByID))
	mux.HandleFunc("/api/assignable-users", server.requireAuth(server.handleAssignableUsers))
	mux.HandleFunc("/api/transfers", server.requireAuth(server.handleTransfers))
	mux.HandleFunc("/api/notifications", server.requireAuth(server.handleNotifications))
	mux.HandleFunc("/api/notifications/read", server.requireAuth(server.handleNotificationsRead))
	mux.HandleFunc("/api/dashboard", server.requireAuth(server.handleDashboard))
	mux.HandleFunc("/api/report", server.requireAuth(server.handleReport))
	// Realtime stream — cookie or Bearer (no query-string tokens).
	mux.HandleFunc("/api/events", withCORS(server.handleEvents))

	// Listen address — change PORT in backend/.env without rebuilding.
	// HOST defaults to 0.0.0.0 so the API is reachable on the server LAN/public IP.
	port := envOr("PORT", "9080")
	host := envOr("HOST", "0.0.0.0")
	addr := host + ":" + port
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           telemetryMiddleware(server.telemetry, recoverMiddleware(server.telemetry, mux)),
		ReadHeaderTimeout: 5 * time.Second,
		// Allow screenshot uploads (up to 5 MB) on slower links.
		ReadTimeout: 60 * time.Second,
		// No global WriteTimeout: the SSE handler is long-lived and clears its
		// own write deadline. Per-request work is bounded by statement_timeout
		// and the client's own request timeout.
		WriteTimeout:   0,
		IdleTimeout:    120 * time.Second,
		MaxHeaderBytes: 1 << 20,
	}

	server.telemetry.EmitOne("server_start", "info", "CRM backend started", "/", "BOOT", 0, "")

	go func() {
		log.Printf("LeadFlow backend listening on http://%s (PORT=%s from env)", addr, port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	// Graceful shutdown: stop accepting, drain in-flight requests.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("shutting down…")
	server.telemetry.EmitOne("server_stop", "warn", "CRM backend stopping", "/", "SHUTDOWN", 0, "")
	// Give emitter a moment to flush the shutdown event.
	time.Sleep(150 * time.Millisecond)
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer shutdownCancel()
	if err := httpServer.Shutdown(shutdownCtx); err != nil {
		log.Printf("graceful shutdown failed: %v", err)
	}
}

// recoverMiddleware turns a panic in any handler into a 500 instead of taking
// down the whole process — critical when serving many concurrent users.
func recoverMiddleware(emitter *TelemetryEmitter, next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("panic serving %s %s: %v", r.Method, r.URL.Path, rec)
				if emitter != nil {
					emitter.EmitOne("server_panic", "error", "handler panic", r.URL.Path, r.Method, 500, "")
				}
				defer func() { _ = recover() }() // header may already be sent (SSE)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
