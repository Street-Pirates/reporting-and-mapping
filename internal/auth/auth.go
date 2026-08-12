// Package auth turns oauth2_proxy identity headers into an internal user.
//
// The Go backend implements NO login flow. oauth2_proxy is expected to sit in
// front of it, authenticate the request, and inject the identity via a trusted
// header. The exact header name is configurable (see Config) because it depends
// on the deployed oauth2_proxy config -- confirm it there, do not assume.
package auth

import (
	"context"
	"log"
	"net/http"
	"strings"

	"sightingmap/internal/store"
)

type ctxKey int

const userKey ctxKey = iota

// Config controls how identity is extracted.
type Config struct {
	// IdentityHeaders are tried in order; the first non-empty one is the subject.
	// Default matches a typical oauth2_proxy: X-Forwarded-Email, then -User.
	IdentityHeaders []string
	// DevSubject, if set, is used when no identity header is present. This lets
	// you run locally without oauth2_proxy. MUST be empty in production.
	DevSubject string
}

// DefaultConfig returns the conventional oauth2_proxy header order.
func DefaultConfig() Config {
	return Config{IdentityHeaders: []string{"X-Forwarded-Email", "X-Forwarded-User"}}
}

// Middleware resolves identity on every request, auto-registers new users and
// updates last_seen_at (the heartbeat), then stashes the user in context.
//
// The subject extracted from the header is never persisted: ResolveUser hashes
// it into the user's opaque ID and discards it.
func Middleware(st *store.Store, cfg Config) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			subject := extractSubject(r, cfg)
			if subject == "" {
				http.Error(w, "unauthenticated: missing oauth2_proxy identity header", http.StatusUnauthorized)
				return
			}
			u, err := st.ResolveUser(subject)
			if err != nil {
				log.Printf("auth: resolve user: %v", err)
				http.Error(w, "internal error", http.StatusInternalServerError)
				return
			}
			ctx := context.WithValue(r.Context(), userKey, u)
			next.ServeHTTP(w, r.WithContext(ctx))
		})
	}
}

func extractSubject(r *http.Request, cfg Config) string {
	for _, h := range cfg.IdentityHeaders {
		if v := strings.TrimSpace(r.Header.Get(h)); v != "" {
			return v
		}
	}
	return cfg.DevSubject
}

// UserFrom returns the authenticated user from the request context.
func UserFrom(ctx context.Context) *store.User {
	u, _ := ctx.Value(userKey).(*store.User)
	return u
}
