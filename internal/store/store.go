// Package store contains all datastore access. Every write is INSERT-only
// except the users heartbeat/role columns (see schema.sql for the rationale).
package store

import (
	"context"
	"database/sql"
	"errors"
	"sync"
	"time"
)

// ReconcileRadiusM is the "within 100m" radius for pulling unreconciled
// sightings into a canonical location's history (spec section 3, action #4).
const ReconcileRadiusM = 100.0

// ErrNotFound is returned when a lookup finds no row.
var ErrNotFound = errors.New("not found")

// Store wraps the database handle.
type Store struct {
	db *sql.DB

	// The user-ID hashing seed never changes for the lifetime of a database, so
	// it is read once and cached rather than re-queried on every request.
	seedOnce sync.Once
	seed     string
	seedErr  error
}

// New returns a Store over an already-opened *sql.DB.
func New(db *sql.DB) *Store { return &Store{db: db} }

func nowUTC() string { return time.Now().UTC().Format(time.RFC3339) }

// ---------------------------------------------------------------------------
// Sightings (append-only)
// ---------------------------------------------------------------------------

// SightingInput is a validated request to insert a sighting.
type SightingInput struct {
	UserID      int64
	Lat, Lon    float64
	ObservedAt  time.Time
	Transpired  string
	ToExist     bool
	Medium      string
	Message     string
	Height      string
	Description string
	LocationID  *int64
}

// InsertSighting appends a new sighting.
func (s *Store) InsertSighting(ctx context.Context, in SightingInput) (int64, error) {
	_, err := s.db.ExecContext(ctx, `
		INSERT INTO locations
		(created_by, lat, lon, created_at)
		select $1, round($2, 7), round($3, 7), $4 where not exists
		(select 1 from locations where round(lat, 6)=round($2, 6) and round(lon, 6)=round($3, 6))`, in.UserID, in.Lat, in.Lon, in.ObservedAt.UTC().Format(time.RFC3339))
	if err != nil {
		return 0, err
	}

	var toExist int
	switch in.Transpired {
	case "sighted":
		toExist = 1
	case "missed", "removed":
		toExist = 0
	default:
		return 0, errors.New("Not sent what transpired at sighting")
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sightings
		(reporter_id, observed_at, to_exist, transpired, medium, message, height, description, location_id)
		SELECT $1, $2, case when $3 then 1 else 0 end, $4, $5, $6, $7, $8, locations.id FROM locations WHERE round(lat, 6)=round($9, 6) AND round(lon, 6)=round($10, 6) LIMIT 1`,
		in.UserID, in.ObservedAt.UTC().Format(time.RFC3339), toExist, in.Transpired,
		in.Medium, in.Message, in.Height, in.Description,
		in.Lat, in.Lon,
	)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---------------------------------------------------------------------------
// Canonical locations (append-only creation)
// ---------------------------------------------------------------------------

// InsertCanonical creates a new canonical location.
func (s *Store) InsertCanonical(ctx context.Context, userID int64, lat, lon float64) (int64, error) {
	res, err := s.db.Exec(`
		INSERT INTO locations (lat, lon, created_by, created_at)
		VALUES (?, ?, ?, ?)`,
		lat, lon, userID, nowUTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

func (s *Store) getLocation(id int64) (Sighting, error) {
	var c Sighting
	err := s.db.QueryRow(
		`SELECT locations.id, lat, lon, created_at, max(sightings.observed_at), sightings.message, sightings.medium, sightings.height FROM sightings left join locations on sightings.location_id = locations.id WHERE locations.id = ?`, id,
	).Scan(&c.Location.ID, &c.Location.Lat, &c.Location.Lon, &c.Location.CreatedAt, &c.ObservedAt, &c.Message, &c.Medium, &c.Height)
	if err == sql.ErrNoRows {
		return c, ErrNotFound
	}
	return c, err
}

// ---------------------------------------------------------------------------
// Reconciliation (append-only linkage)
// ---------------------------------------------------------------------------

// InsertReconciliation links an existing sighting to a canonical location.
func (s *Store) InsertReconciliation(ctx context.Context, userID, sightingID, canonicalID int64) (int64, error) {
	// Validate both endpoints exist (FKs also enforce this).
	if _, err := s.getLocation(canonicalID); err != nil {
		return 0, err
	}
	var exists int
	if err := s.db.QueryRowContext(ctx, `SELECT 1 FROM sightings WHERE id = ?`, sightingID).Scan(&exists); err != nil {
		if err == sql.ErrNoRows {
			return 0, ErrNotFound
		}
		return 0, err
	}
	res, err := s.db.Exec(`
		INSERT INTO reconciliations (sighting_id, canonical_location_id, reconciled_by, created_at)
		VALUES (?, ?, ?, ?)`,
		sightingID, canonicalID, userID, nowUTC())
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// ---------------------------------------------------------------------------
// Insincere flagging (append-only) & reputation notes
// ---------------------------------------------------------------------------

// SetInsincere appends an insincere flag (value=1) or clearing (value=0) for the
// target opaque user id. Append-only: history is preserved, latest row wins.
func (s *Store) SetInsincere(ctx context.Context, byUserID int64, targetOpaque string, insincere bool) error {
	targetID, err := s.userByOpaque(ctx, targetOpaque)
	if err != nil {
		return err
	}
	value := -1
	if insincere {
		value = 1
	}
	_, err = s.db.Exec(`
		INSERT INTO user_flags (target_user_id, flag_type, value, flagged_by, created_at)
		VALUES (?, 'insincere', ?, ?, ?)`,
		targetID, value, byUserID, nowUTC())
	return err
}

// AddNote appends a reputation note about the target opaque user id.
func (s *Store) AddNote(ctx context.Context, authorUserID int64, targetOpaque, note string) error {
	targetID, err := s.userByOpaque(ctx, targetOpaque)
	if err != nil {
		return err
	}
	_, err = s.db.ExecContext(ctx, `
		INSERT INTO reputation_notes (target_user_id, author_user_id, note, created_at)
		VALUES (?, ?, ?, ?)`,
		targetID, authorUserID, note, nowUTC())
	return err
}

// ListNotes returns reputation notes about the target opaque user id (audit-scoped).
func (s *Store) ListNotes(ctx context.Context, targetOpaque string) ([]Note, error) {
	targetID, err := s.userByOpaque(ctx, targetOpaque)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.Query(`
		SELECT n.id, a.opaque_id, n.note, n.created_at
		FROM reputation_notes n JOIN users a ON a.id = n.author_user_id
		WHERE n.target_user_id = ?
		ORDER BY n.created_at DESC, n.id DESC`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Note
	for rows.Next() {
		var n Note
		if err := rows.Scan(&n.ID, &n.AuthorOpaqueID, &n.Note, &n.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, n)
	}
	return out, rows.Err()
}

func (s *Store) ListUserFlags(ctx context.Context, targetOpaque string) ([]UserFlag, error) {
	targetID, err := s.userByOpaque(ctx, targetOpaque) // TODO: change to JOIN
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, `
		SELECT uf.id, uf.flag_type, uf.value, a.opaque_id, uf.created_at
		FROM user_flags uf
		JOIN users a ON a.id = uf.flagged_by
		WHERE uf.target_user_id = ?
		ORDER BY uf.created_at DESC, uf.id DESC`, targetID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []UserFlag
	for rows.Next() {
		var f UserFlag
		if err := rows.Scan(&f.ID, &f.FlagType, &f.Value, &f.FlaggedByOpaqueID, &f.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, f)
	}
	return out, rows.Err()
}
