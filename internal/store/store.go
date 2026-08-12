// Package store contains all datastore access. Every write is INSERT-only
// except the users heartbeat/role columns (see schema.sql for the rationale).
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sync"
	"time"

	"sightingmap/internal/identity"
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

// hashSeed returns the cached config.user_id_hash_seed. db.Open writes it at
// startup, so a missing row means the database was not opened through that path.
func (s *Store) hashSeed() (string, error) {
	s.seedOnce.Do(func() {
		s.seed, s.seedErr = s.Config(identity.SeedConfigKey)
		if s.seedErr == ErrNotFound {
			s.seedErr = fmt.Errorf("%s is missing from config; open the database with db.Open", identity.SeedConfigKey)
		}
	})
	return s.seed, s.seedErr
}

// ---------------------------------------------------------------------------
// Users & permissions
// ---------------------------------------------------------------------------

// ResolveUser maps an oauth2_proxy subject to an internal user, auto-registering
// on first sight and always bumping last_seen_at (the heartbeat).
//
// The subject is never stored: it is hashed into the row's opaque_id (see
// internal/identity) and discarded. This is the only place it is hashed.
func (s *Store) ResolveUser(subject string) (*User, error) {
	seed, err := s.hashSeed()
	if err != nil {
		return nil, err
	}
	opaque, err := identity.OpaqueUserID(seed, subject)
	if err != nil {
		return nil, err
	}

	// One statement registers on first sight and bumps the heartbeat otherwise.
	// The UNIQUE constraint on opaque_id makes it race-free between concurrent
	// requests for the same new user.
	now := nowUTC()
	var u User
	err = s.db.QueryRow(`
		INSERT INTO users (opaque_id, role, registered_at, last_seen_at)
		VALUES (?, 'reporter', ?, ?)
		ON CONFLICT(opaque_id) DO UPDATE SET last_seen_at = excluded.last_seen_at
		RETURNING id, opaque_id, role, registered_at, last_seen_at, called`,
		opaque, now, now,
	).Scan(&u.ID, &u.OpaqueID, &u.Role, &u.RegisteredAt, &u.LastSeenAt, &u.Called)
	if err != nil {
		return nil, err
	}
	return &u, nil
}

// HasPermission consults the role_permissions table (no hardcoded logic).
func (s *Store) HasPermission(role, permission string) (bool, error) {
	var one int
	err := s.db.QueryRow(
		`SELECT 1 FROM role_permissions WHERE role = ? AND permission = ?`,
		role, permission,
	).Scan(&one)
	if err == sql.ErrNoRows {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, nil
}

// userByOpaque resolves a public opaque ID to an internal id.
func (s *Store) userByOpaque(ctx context.Context, opaqueID string) (int64, error) {
	var id int64
	err := s.db.QueryRowContext(ctx, `SELECT id FROM users WHERE opaque_id = ?`, opaqueID).Scan(&id)
	if err == sql.ErrNoRows {
		return 0, ErrNotFound
	}
	return id, err
}

// ListUsers returns every user with its opaque id and role.
func (s *Store) ListUsers() ([]User, error) {
	rows, err := s.db.Query(`SELECT id, opaque_id, role, registered_at, last_seen_at, called FROM users ORDER BY id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []User
	for rows.Next() {
		var u User
		if err := rows.Scan(&u.ID, &u.OpaqueID, &u.Role, &u.RegisteredAt, &u.LastSeenAt, &u.Called); err != nil {
			return nil, err
		}
		out = append(out, u)
	}
	return out, rows.Err()
}

// SetUserRole updates the specified user's role.
func (s *Store) SetUserRole(opaqueID, role string) error {
	res, err := s.db.Exec(`UPDATE users SET role = ? WHERE opaque_id = ?`, role, opaqueID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

func (s *Store) SetUserCalled(ctx context.Context, userID int64, called string) error {
	res, err := s.db.Exec(`UPDATE users SET called = ? WHERE id = ?`, called, userID)
	if err != nil {
		return err
	}
	rows, err := res.RowsAffected()
	if err != nil {
		return err
	}
	if rows == 0 {
		return ErrNotFound
	}
	return nil
}

// ---------------------------------------------------------------------------
// Sightings (append-only)
// ---------------------------------------------------------------------------

// SightingInput is a validated request to insert a sighting.
type SightingInput struct {
	UserID      int64
	Lat, Lon    float64
	ObservedAt  time.Time
	ToExist     bool
	Medium      string
	Message     string
	Height      string
	Description string
	LocationID  *int64
}

// InsertSighting appends a new sighting.
func (s *Store) InsertSighting(ctx context.Context, in SightingInput) (int64, error) {
	if in.LocationID == nil {
		_, err := s.db.ExecContext(ctx, `
		INSERT INTO locations
		(created_by, lat, lon, created_at)
		select $1, $2, $3, $4 where not exists
		(select 1 from locations where lat=$2 and lon=$3)`, in.UserID, in.Lat, in.Lon, in.ObservedAt.UTC().Format(time.RFC3339))
		if err != nil {
			return 0, err
		}
	}

	res, err := s.db.ExecContext(ctx, `
		INSERT INTO sightings
		(reporter_id, observed_at, to_exist, medium, message, height, description, location_id)
		values ($1, $2, case when $3 then 1 else 0 end, $4, $5, $6, $7, $8)`,
		in.UserID, in.ObservedAt.UTC().Format(time.RFC3339), in.ToExist,
		in.Medium, in.Message, in.Height, in.Description, in.LocationID,
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

func (s *Store) getLocation(id int64) (Location, error) {
	var c Location
	err := s.db.QueryRow(
		`SELECT id, lat, lon, created_at FROM locations WHERE id = ?`, id,
	).Scan(&c.ID, &c.Lat, &c.Lon, &c.CreatedAt)
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
