package main

import (
	"context"
	"log"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"
)

func main() {
	loadEnvFromCRM()
	corsConfig = loadCORSConfig()

	jwtSecret := envOr("JWT_SECRET", "")
	if jwtSecret == "" {
		log.Fatal("JWT_SECRET is required")
	}

	crmDB := envOr("DATABASE_URL", "")
	telDB := envOr("TELEMETRY_DATABASE_URL", "")
	// Prefer explicit telemetry DB; otherwise same Postgres with support_telemetry schema
	// (avoids requiring CREATE DATABASE privileges on shared hosts).
	if telDB == "" {
		telDB = crmDB
	}

	ctx := context.Background()
	pool, usedURL, err := connectStore(ctx, telDB, crmDB)
	if err != nil {
		log.Fatalf("telemetry database: %v", err)
	}
	defer pool.Close()
	log.Printf("telemetry store connected (%s)", redactURL(usedURL))

	store := NewStore(pool)
	migrateCtx, cancel := context.WithTimeout(ctx, 60*time.Second)
	if err := RunTelemetryMigrations(migrateCtx, pool); err != nil {
		cancel()
		log.Fatalf("migrations: %v", err)
	}
	cancel()

	presence := NewPresence(envOr("REDIS_URL", ""))
	defer presence.Close()

	crmBase := envOr("CRM_URL", envOr("TELEMETRY_CRM_URL", "http://127.0.0.1:9080"))
	poller := NewHealthPoller(store, crmBase)
	runCtx, runCancel := context.WithCancel(context.Background())
	defer runCancel()
	poller.Start(runCtx)

	ingestKey := envOr("TELEMETRY_INGEST_SECRET", envOr("JWT_SECRET", ""))
	server := &Server{
		store:     store,
		tokens:    NewTokenService(jwtSecret),
		presence:  presence,
		poller:    poller,
		ingestKey: ingestKey,
	}

	// Record telemetry process start.
	_, _ = store.InsertEvents(ctx, []IngestEvent{{
		Kind:     "server_start",
		Severity: "info",
		Source:   "telemetry",
		Message:  "telemetry service started",
	}})

	mux := http.NewServeMux()
	mux.HandleFunc("/health", withCORS(server.handleHealth))
	mux.HandleFunc("/v1/ingest", withCORS(server.handleIngest))
	mux.HandleFunc("/v1/heartbeat", server.requireAuth(server.handleHeartbeat))
	mux.HandleFunc("/v1/support/overview", server.requireSupportRead(server.handleOverview))
	mux.HandleFunc("/v1/support/events", server.requireSupportRead(server.handleEvents))

	port := envOr("TELEMETRY_PORT", "9081")
	host := envOr("TELEMETRY_HOST", envOr("HOST", "0.0.0.0"))
	addr := host + ":" + port
	httpServer := &http.Server{
		Addr:              addr,
		Handler:           recoverMW(mux),
		ReadHeaderTimeout: 5 * time.Second,
		ReadTimeout:       15 * time.Second,
		WriteTimeout:      30 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	go func() {
		log.Printf("LeadFlow telemetry listening on http://%s (TELEMETRY_PORT=%s)", addr, port)
		if err := httpServer.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatal(err)
		}
	}()

	stop := make(chan os.Signal, 1)
	signal.Notify(stop, os.Interrupt, syscall.SIGTERM)
	<-stop
	log.Println("telemetry shutting down…")
	_, _ = store.InsertEvents(context.Background(), []IngestEvent{{
		Kind:     "server_stop",
		Severity: "warn",
		Source:   "telemetry",
		Message:  "telemetry service stopping",
	}})
	shutdownCtx, shutdownCancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer shutdownCancel()
	runCancel()
	_ = httpServer.Shutdown(shutdownCtx)
}

func recoverMW(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		defer func() {
			if rec := recover(); rec != nil {
				log.Printf("telemetry panic %s %s: %v", r.Method, r.URL.Path, rec)
				writeError(w, http.StatusInternalServerError, "internal server error")
			}
		}()
		next.ServeHTTP(w, r)
	})
}
