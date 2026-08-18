package store

// User is the internal representation. Only OpaqueID is ever serialized to
// clients; ID stays server-side. The underlying oauth2_proxy subject is not a
// field here because it is never stored -- OpaqueID is derived from it (see
// internal/identity).
type User struct {
	ID           int64  `json:"-"`
	OpaqueID     string `json:"opaque_id"`
	Role         string `json:"role"`
	RegisteredAt string `json:"registered_at"`
	LastSeenAt   string `json:"last_seen_at"`
	Called       string `json:"called"`
}

// Report types.
const (
	ReportNewExistence       = "new_existence"
	ReportNonExistence       = "non_existence"
	ReportContinuedExistence = "continued_existence"
)

// Permission names (gate API actions via the role_permissions table).
const (
	PermSight         = "sight"
	PermReconcile     = "reconcile"
	PermCanonicalize  = "canonicalize"
	PermView          = "view"
	PermAudit         = "audit"
	PermGossip        = "gossip"
	PermFlagInsincere = "flag_insincere"
	PermSetUserFlags  = "set-user-flags"
)

// KnownRoles returns the canonical role names used by the application.
func KnownRoles() []string {
	return []string{"reporter", "editor", "administrator", "muted"}
}

// Location is a trusted, reconciled sign location.
type Location struct {
	ID          int64   `json:"location_id"`
	PlusCode    string  `json:"pluscode"`
	Lat         float64 `json:"lat"`
	Lon         float64 `json:"lon"`
	ImageURL    *string `json:"image_url"`
	Description string  `json:"location_description"`
	CreatedAt   string  `json:"created_at"`
}

// Sighting is a raw, append-only observation.
type Sighting struct {
	Location
	ID             int64   `json:"sighting_id"`
	AuthorOpaqueID string  `json:"author_opaque_id"`
	Lat            float64 `json:"lat"`
	Lon            float64 `json:"lon"`
	ObservedAt     string  `json:"observed_at"`
	ToExist        bool    `json:"to_exist"`
	Description    string  `json:"description,omitempty"`
	Medium         string  `json:"medium"`
	Message        string  `json:"message"`
	Height         string  `json:"height"`
	LocationID     *int64  `json:"location_id,omitempty"`
	// DistanceM is set (>=0) only for unreconciled sightings returned as part of
	// a canonical location's history (the "within 100m" set).
	DistanceM *float64 `json:"distance_m,omitempty"`
}

// MapMarker is a canonical location decorated with its current derived status.
type MapMarker struct {
	Location
	ID               int64  `json:"sighting_id"`
	Exists           bool   `json:"exists"` // false if latest report is non_existence
	Title            string `json:"title"`
	Medium           string `json:"medium"`
	Message          string `json:"message"`
	Height           string `json:"height"`
	LatestObservedAt string `json:"latest_observed_at"`
	SightingCount    int    `json:"sighting_count"`
}

type LocationHistory struct {
	Location    Sighting   `json:"location"`
	Reconciled  []Sighting `json:"reconciled"`
	NearbyUnrec []Sighting `json:"nearby_unrec"`
}

// AuditEvent represents either a saved sighting or a location creation event.
type AuditEvent struct {
	EventType         string  `json:"event_type"`
	EventID           int64   `json:"event_id"`
	AuthorOpaqueID    string  `json:"author_opaque_id"`
	AuthorInsincerity int     `json:"author_insincerity"`
	Lat               float64 `json:"lat"`
	Lon               float64 `json:"lon"`
	Timestamp         string  `json:"timestamp"`
	ToExist           *bool   `json:"to_exist,omitempty"`
	Transpired        *string `json:"transpired"`
	Description       *string `json:"description,omitempty"`
	Medium            *string `json:"medium,omitempty"`
	Message           *string `json:"message,omitempty"`
	Height            *string `json:"height,omitempty"`
	LocationID        *int64  `json:"location_id,omitempty"`
}

// MapData is what the frontend renders.
type MapData struct {
	Markers  []MapMarker `json:"markers"`
	Proposed []Sighting  `json:"proposed"` // unreconciled new_existence reports
}

// Note is a reputation ("gossip") note.
type Note struct {
	ID             int64  `json:"id"`
	AuthorOpaqueID string `json:"author_opaque_id"`
	Note           string `json:"note"`
	CreatedAt      string `json:"created_at"`
}

type UserFlag struct {
	ID                int64  `json:"id"`
	FlagType          string `json:"flag_type"`
	Value             int    `json:"value"`
	FlaggedByOpaqueID string `json:"flagged_by_opaque_id"`
	CreatedAt         string `json:"created_at"`
}
