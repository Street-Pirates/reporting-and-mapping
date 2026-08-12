package api

import (
	"net/http"
	"strconv"
	"time"

	"sightingmap/internal/auth"
	"sightingmap/internal/store"
)

// GET /api/me — heartbeat + whoami. Any authenticated user. last_seen_at is
// already updated by the auth middleware on every request.
func (s *Server) handleMe(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	writeJSON(w, http.StatusOK, u)
}

// POST /api/me — update the current user's called note from ?called= or JSON body.
func (s *Server) handleUpdateMe(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	called := r.URL.Query().Get("called")
	var body struct {
		Called string `json:"called"`
	}
	if err := decodeOptional(r, &body); err != nil {
		writeErr(w, http.StatusBadRequest, "invalid request body")
		return
	}
	if body.Called != "" {
		called = body.Called
	}
	if err := s.st.SetUserCalled(r.Context(), u.ID, called); err != nil {
		mapStoreErr(w, err)
		return
	}
	u.Called = called
	writeJSON(w, http.StatusOK, u)
}

// POST /api/sightings — requires `sight`.
// Body: {lat, lon, observed_at?, to_exist, medium?, message?, height?, location_id?}
func (s *Server) handleCreateSighting(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermSight) {
		return
	}
	var body struct {
		Lat                 float64 `json:"lat"`
		Lon                 float64 `json:"lon"`
		ObservedAt          string  `json:"observed_at"`
		ToExist             bool    `json:"to_exist"`
		Medium              string  `json:"medium"`
		Message             string  `json:"message"`
		Height              string  `json:"height"`
		CanonicalLocationID *int64  `json:"location_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validLatLon(body.Lat, body.Lon) {
		writeErr(w, http.StatusBadRequest, "lat/lon out of range")
		return
	}
	if body.Medium == "" {
		body.Medium = "unknown"
	}
	switch body.Medium {
	case "unknown", "placard", "ink", "sticker":
	default:
		writeErr(w, http.StatusBadRequest, "invalid medium")
		return
	}
	if body.Message == "" {
		body.Message = "unknown"
	}
	switch body.Message {
	case "unknown", "js", "jicr", "bij", "other":
	default:
		writeErr(w, http.StatusBadRequest, "invalid message")
		return
	}
	if body.Height == "" {
		body.Height = "unknown"
	}
	switch body.Height {
	case "unknown", "reachable", "7ft", "10ft", "15+ft":
	default:
		writeErr(w, http.StatusBadRequest, "invalid height")
		return
	}
	switch body.ToExist {
	case true, false:
	default:
		writeErr(w, http.StatusBadRequest, "invalid to_exist")
		return
	}
	observed := time.Now().UTC()
	if body.ObservedAt != "" {
		t, err := time.Parse(time.RFC3339, body.ObservedAt)
		if err != nil {
			writeErr(w, http.StatusBadRequest, "observed_at must be RFC-3339")
			return
		}
		observed = t.UTC()
	}

	id, err := s.st.InsertSighting(r.Context(), store.SightingInput{
		UserID:     u.ID,
		Lat:        body.Lat,
		Lon:        body.Lon,
		ObservedAt: observed,
		ToExist:    body.ToExist,
		Medium:     body.Medium,
		Message:    body.Message,
		Height:     body.Height,
		LocationID: body.CanonicalLocationID,
	})
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// POST /api/locations — requires `canonicalize`.
// Body: {lat, lon, description?}
func (s *Server) handleCreateLocation(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermCanonicalize) {
		return
	}
	var body struct {
		Lat         float64 `json:"lat"`
		Lon         float64 `json:"lon"`
		Description string  `json:"description"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if !validLatLon(body.Lat, body.Lon) {
		writeErr(w, http.StatusBadRequest, "lat/lon out of range")
		return
	}
	id, err := s.st.InsertCanonical(r.Context(), u.ID, body.Lat, body.Lon)
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// GET /api/locations/{id}/history — requires `view`.
// Query: days (default 365) or since; include_insincere.
func (s *Server) handleLocationHistory(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermView) {
		return
	}
	id, err := strconv.ParseInt(r.PathValue("id"), 10, 64)
	if err != nil {
		writeErr(w, http.StatusBadRequest, "invalid canonical location id")
		return
	}
	hist, err := s.st.LocationHistory(r.Context(), id, parseSince(r, 365), includeInsincere(r))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, hist)
}

// POST /api/reconciliations — requires `reconcile`.
// Body: {sighting_id, location_id}
func (s *Server) handleReconcile(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermReconcile) {
		return
	}
	var body struct {
		SightingID int64 `json:"sighting_id"`
		LocationID int64 `json:"location_id"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.SightingID == 0 || body.LocationID == 0 {
		writeErr(w, http.StatusBadRequest, "sighting_id and location_id required")
		return
	}
	id, err := s.st.InsertReconciliation(r.Context(), u.ID, body.SightingID, body.LocationID)
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, map[string]int64{"id": id})
}

// GET /api/map — requires `view`. Query: days (default 90) or since; include_insincere.
func (s *Server) handleMap(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermView) {
		return
	}
	bbox, err := parseBBox(r)
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	data, err := s.st.MapData(r.Context(), parseSince(r, 90), includeInsincere(r), includeMissing(r), bbox)
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, data)
}

// GET /api/users/{opaque}/sightings — requires `audit`.
func (s *Server) handleAuditSightings(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermAudit) {
		return
	}
	sightings, err := s.st.SightingsByOpaqueUser(r.Context(), r.PathValue("opaque"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"sightings": sightings})
}

// GET /api/events — returns recent sightings for audit UI.
// Query: horizon_days (integer) or since (RFC-3339). Defaults to 14 days.
func (s *Server) handleListEvents(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermAudit) {
		return
	}
	// Default to 14 days unless since provided.
	var since time.Time
	if sparam := r.URL.Query().Get("since"); sparam != "" {
		if t, err := time.Parse(time.RFC3339, sparam); err == nil {
			since = t.UTC()
		} else {
			writeErr(w, http.StatusBadRequest, "since must be RFC-3339")
			return
		}
	} else if h := r.URL.Query().Get("horizon_days"); h != "" {
		if v, err := strconv.ParseFloat(h, 64); err == nil && v > 0 {
			if v > 3650 {
				v = 3650
			}
			since = time.Now().UTC().Add(-time.Duration(v * float64(24*time.Hour)))
		} else {
			writeErr(w, http.StatusBadRequest, "invalid horizon_days")
			return
		}
	} else {
		since = time.Now().UTC().Add(-14 * 24 * time.Hour)
	}

	events, err := s.st.AuditEventsSince(r.Context(), since)
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// GET /api/sightings — returns recent sightings for audit UI.
// Query: horizon_days (integer) or since (RFC-3339). Defaults to 14 days.
func (s *Server) handleListSightings(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermAudit) {
		return
	}
	// Default to 14 days unless since provided.
	var since time.Time
	if sparam := r.URL.Query().Get("since"); sparam != "" {
		if t, err := time.Parse(time.RFC3339, sparam); err == nil {
			since = t.UTC()
		} else {
			writeErr(w, http.StatusBadRequest, "since must be RFC-3339")
			return
		}
	} else if h := r.URL.Query().Get("horizon_days"); h != "" {
		if v, err := strconv.ParseFloat(h, 64); err == nil && v > 0 {
			if v > 3650 {
				v = 3650
			}
			since = time.Now().UTC().Add(-time.Duration(v * float64(24*time.Hour)))
		} else {
			writeErr(w, http.StatusBadRequest, "invalid horizon_days")
			return
		}
	} else {
		since = time.Now().UTC().Add(-14 * 24 * time.Hour)
	}

	events, err := s.st.SightingsSince(r.Context(), since)
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, events)
}

// POST /api/users/{opaque}/insincere — requires `flag_insincere`.
// Body: {insincere: bool}  (defaults to true if omitted)
func (s *Server) handleFlagInsincere(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermFlagInsincere) {
		return
	}
	body := struct {
		Insincere *bool `json:"insincere"`
	}{}
	// Body is optional; ignore decode errors on empty body.
	_ = decodeOptional(r, &body)
	insincere := true
	if body.Insincere != nil {
		insincere = *body.Insincere
	}
	if err := s.st.SetInsincere(r.Context(), u.ID, r.PathValue("opaque"), insincere); err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]bool{"insincere": insincere})
}

// GET /api/users/{opaque}/flags — requires `audit`.
func (s *Server) handleListFlags(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermAudit) {
		return
	}
	flags, err := s.st.ListUserFlags(r.Context(), r.PathValue("opaque"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"flags": flags})
}

// POST /api/users/{opaque}/flags — requires `set-user-flags`.
func (s *Server) handleSetFlags(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermSetUserFlags) {
		return
	}
	var body struct {
		FlagType string `json:"flag_type"`
		Value    *int   `json:"value"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.FlagType != "insincere" {
		writeErr(w, http.StatusBadRequest, "unsupported flag_type")
		return
	}
	value := 1
	if body.Value != nil {
		value = *body.Value
	}
	if value != 0 && value != 1 {
		writeErr(w, http.StatusBadRequest, "value must be 0 or 1")
		return
	}
	if err := s.st.SetInsincere(r.Context(), u.ID, r.PathValue("opaque"), value == 1); err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]int{"value": value})
}

// POST /api/users/{opaque}/notes — requires `gossip`.
// Body: {note}
func (s *Server) handleAddNote(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermGossip) {
		return
	}
	var body struct {
		Note string `json:"note"`
	}
	if !decodeJSON(w, r, &body) {
		return
	}
	if body.Note == "" {
		writeErr(w, http.StatusBadRequest, "note required")
		return
	}
	if err := s.st.AddNote(r.Context(), u.ID, r.PathValue("opaque"), body.Note); err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusCreated, nil)
}

// GET /api/users/{opaque}/notes — requires `audit` (read is audit-scoped).
func (s *Server) handleListNotes(w http.ResponseWriter, r *http.Request) {
	u := auth.UserFrom(r.Context())
	if !s.require(w, u, store.PermAudit) {
		return
	}
	notes, err := s.st.ListNotes(r.Context(), r.PathValue("opaque"))
	if err != nil {
		mapStoreErr(w, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"notes": notes})
}

func (s *Server) handlePing(w http.ResponseWriter, r *http.Request) {
	w.WriteHeader(http.StatusOK)
}
