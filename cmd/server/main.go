// Command server runs the Ad-Sighting Tracker backend + frontend.
//
// It expects oauth2_proxy in front of it, injecting identity via a header
// (configurable, see AUTH_HEADERS). It never implements its own login.
package main

import (
	"context"
	"flag"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"sightingmap/internal/api"
	"sightingmap/internal/auth"
	"sightingmap/internal/db"
	"sightingmap/internal/store"
	"sightingmap/internal/webui"
)

func main() {
	// bypassedAuthEmail bypasses oauth2_proxy by treating the request as coming
	// from this user. Defaults to DEV_SUBJECT so existing env-based setups keep
	// working; the flag takes precedence when passed. Local dev only — never in prod.
	bypassedAuthEmail := flag.String("bypassed-auth-email", os.Getenv("DEV_SUBJECT"),
		"treat the authenticated user as having this email address (bypasses oauth2_proxy); local dev only")
	flag.Parse()

	var addr, dbPath string
	var found bool
	if addr, found = os.LookupEnv("ADDR"); !found {
		addr = ":8080"
	}

	if dbPath, found = os.LookupEnv("DB_PATH"); !found {
		dbPath = "sightingmap.db"
	}

	sqlDB, err := db.Open(dbPath)
	if err != nil {
		log.Fatalf("open db: %v", err)
	}
	defer sqlDB.Close()
	st := store.New(sqlDB)

	authCfg := auth.DefaultConfig()
	if h := os.Getenv("AUTH_HEADERS"); h != "" {
		authCfg.IdentityHeaders = splitCSV(h)
	}
	// -bypassed-auth-email (or DEV_SUBJECT) lets you run without oauth2_proxy
	// locally. Leave empty in prod.
	authCfg.DevSubject = *bypassedAuthEmail
	if authCfg.DevSubject != "" {
		log.Printf("WARNING: -bypassed-auth-email set (%q) — auth is bypassed; do not use in production", authCfg.DevSubject)
	}

	srv := api.New(st, webui.FS())
	handler := auth.Middleware(st, authCfg)(srv.Routes())
	handler = api.PanicRecoveryMiddleware(handler)
	handler = api.RequestLoggingMiddleware(handler)

	httpSrv := &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       10 * time.Second,
		WriteTimeout:      10 * time.Second,
	}

	go func() {
		log.Printf("listening on %s (db=%s, auth headers=%v)", addr, dbPath, authCfg.IdentityHeaders)
		if err := httpSrv.ListenAndServe(); err != nil && err != http.ErrServerClosed {
			log.Fatalf("serve: %v", err)
		}
	}()

	// Graceful shutdown.
	stop := make(chan os.Signal, 1)
	signal.Notify(stop, syscall.SIGINT, syscall.SIGTERM)
	<-stop
	log.Println("shutting down...")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(ctx); err != nil {
		log.Printf("shutdown: %v", err)
	}
}

func splitCSV(s string) []string {
	parts := strings.Split(s, ",")
	out := parts[:0]
	for _, p := range parts {
		if t := strings.TrimSpace(p); t != "" {
			out = append(out, t)
		}
	}
	return out
}
