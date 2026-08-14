// Package api wires the REST endpoints. Every endpoint documents the permission
// it requires; gating is enforced via Store.HasPermission (role_permissions).
package api

import (
	"encoding/json"
	"errors"
	"io/fs"
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"sightingmap/internal/geo"
	"sightingmap/internal/store"
)

// Server holds handler dependencies.
type Server struct {
	st     *store.Store
	static fs.FS
}

// New returns a Server. static is the filesystem for the frontend (web/).
func New(st *store.Store, static fs.FS) *Server {
	return &Server{st: st, static: static}
}

// Routes returns the fully-wired handler.
//
//	Endpoint                                   Method  Permission
//	-------------------------------------------------------------------
//	/api/me                                    GET     (any authenticated) heartbeat
//	/api/me                                    POST    (any authenticated) update called note
//	/api/sightings                             POST    sight
//	/api/locations                             POST    canonicalize
//	/api/locations/{id}/history                GET     view
//	/api/reconciliations                       POST    reconcile
//	/api/map                                   GET     view
//	/api/users/{opaque}/sightings             GET     audit
//	/api/users/{opaque}/insincere             POST    flag_insincere (role-gated)
//	/api/users/{opaque}/flags                 GET     audit
//	/api/users/{opaque}/flags                 POST    set-user-flags (role-gated)
//	/api/users/{opaque}/notes                 POST    gossip
//	/api/users/{opaque}/notes                 GET     audit
func (s *Server) Routes() http.Handler {
	mux := http.NewServeMux()

	mux.HandleFunc("GET /api/me", s.handleMe)
	mux.HandleFunc("POST /api/me", s.handleUpdateMe)
	mux.HandleFunc("GET /api/sightings", s.handleListSightings)
	mux.HandleFunc("GET /api/events", s.handleListEvents)
	mux.HandleFunc("POST /api/sightings", s.handleCreateSighting)
	mux.HandleFunc("POST /api/locations", s.handleCreateLocation)
	mux.HandleFunc("GET /api/locations/{id}/history", s.handleLocationHistory)
	mux.HandleFunc("POST /api/reconciliations", s.handleReconcile)
	mux.HandleFunc("GET /api/map", s.handleMap)
	mux.HandleFunc("GET /api/users/{opaque}/sightings", s.handleAuditSightings)
	mux.HandleFunc("GET /api/users/{opaque}/flags", s.handleListFlags)
	mux.HandleFunc("POST /api/users/{opaque}/flags", s.handleSetFlags)
	mux.HandleFunc("POST /api/users/{opaque}/insincere", s.handleFlagInsincere)
	mux.HandleFunc("POST /api/users/{opaque}/notes", s.handleAddNote)
	mux.HandleFunc("GET /api/users/{opaque}/notes", s.handleListNotes)
	mux.HandleFunc("GET /ping", s.handlePing)

	// Static frontend.
	fileServeHandler := http.FileServer(http.FS(s.static))
	mux.HandleFunc("/", func(w http.ResponseWriter, r *http.Request) {
		if !strings.Contains(r.URL.Path, ".") && !strings.HasSuffix(r.URL.Path, "/") {
			r.URL.Path = r.URL.Path + ".html"
		}
		fileServeHandler.ServeHTTP(w, r)
	})
	return mux
}

// --- helpers ---------------------------------------------------------------

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	if v != nil {
		if err := json.NewEncoder(w).Encode(v); err != nil {
			log.Printf("api: encode: %v", err)
		}
	}
}

func writeErr(w http.ResponseWriter, status int, msg string) {
	writeJSON(w, status, map[string]string{"error": msg})
}

// require checks a permission and writes 403 if missing. Returns true if allowed.
func (s *Server) require(w http.ResponseWriter, u *store.User, perm string) bool {
	ok, err := s.st.HasPermission(u.Role, perm)
	if err != nil {
		writeErr(w, http.StatusInternalServerError, "permission check failed")
		return false
	}
	if !ok {
		writeErr(w, http.StatusForbidden, "missing permission: "+perm)
		return false
	}
	return true
}

// parseSince derives the time-back window start from ?days= (float) or ?since=
// (RFC-3339). Falls back to defaultDays. Clamped to [0.001, 3650] days.
func parseSince(r *http.Request, defaultDays float64) time.Time {
	if s := r.URL.Query().Get("since"); s != "" {
		if t, err := time.Parse(time.RFC3339, s); err == nil {
			return t.UTC()
		}
	}
	days := defaultDays
	if d := r.URL.Query().Get("days"); d != "" {
		if v, err := strconv.ParseFloat(d, 64); err == nil && v > 0 {
			days = v
		}
	}
	if days < 0.001 {
		days = 0.001
	}
	if days > 3650 {
		days = 3650
	}
	return time.Now().UTC().Add(-time.Duration(days * float64(24*time.Hour)))
}

func includeInsincere(r *http.Request) bool {
	v := r.URL.Query().Get("include_insincere")
	return v == "1" || v == "true"
}

// includeMissing reports whether the client asked to also see reported-missing
// (latest non_existence) markers. Defaults to false: present-only.
func includeMissing(r *http.Request) bool {
	v := r.URL.Query().Get("include_gones")
	return v == "1" || v == "true"
}

// parseBBox reads an optional viewport bounding box from the min_lat/max_lat/
// min_lon/max_lon query params. It returns nil (meaning "no spatial filter")
// unless all four parse and form a well-ordered, in-range box.
func parseBBox(r *http.Request) (*geo.BBox, error) {
	q := r.URL.Query()
	minLat, e1 := strconv.ParseFloat(q.Get("min_lat"), 64)
	maxLat, e2 := strconv.ParseFloat(q.Get("max_lat"), 64)
	minLon, e3 := strconv.ParseFloat(q.Get("min_lon"), 64)
	maxLon, e4 := strconv.ParseFloat(q.Get("max_lon"), 64)
	if e1 != nil || e2 != nil || e3 != nil || e4 != nil {
		return nil, errors.Join(e1, e2, e3, e4)
	}
	if minLat > maxLat || minLon > maxLon {
		return nil, errors.New("invalid bounding box")
	}
	// The client expands the viewport 4x, so edges can spill past the valid
	// range near the poles / antimeridian; clamp rather than drop the filter.
	return &geo.BBox{
		MinLat: clamp(minLat, -90, 90), MaxLat: clamp(maxLat, -90, 90),
		MinLon: clamp(minLon, -180, 180), MaxLon: clamp(maxLon, -180, 180),
	}, nil
}

func clamp(v, lo, hi float64) float64 {
	if v < lo {
		return lo
	}
	if v > hi {
		return hi
	}
	return v
}

func decodeJSON(w http.ResponseWriter, r *http.Request, dst any) bool {
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 1<<20)).Decode(dst); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid JSON body: "+err.Error())
		return false
	}
	return true
}

// mapStoreErr maps common store errors to HTTP status codes.
func mapStoreErr(w http.ResponseWriter, err error) {
	if errors.Is(err, store.ErrNotFound) {
		writeErr(w, http.StatusNotFound, "not found")
		return
	}
	log.Printf("api: store error: %v", err)
	writeErr(w, http.StatusInternalServerError, "internal error")
}
