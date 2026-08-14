package store

import (
	"context"
	"database/sql"
	"fmt"
	"sightingmap/internal/identity"
)

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
