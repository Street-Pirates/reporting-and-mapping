// Package db opens the SQLite datastore and applies the schema at startup.
package db

import (
	"database/sql"
	_ "embed"
	"fmt"
	"log"
	"time"

	"sightingmap/internal/identity"

	_ "modernc.org/sqlite" // pure-Go driver, no cgo
)

//go:embed schema.sql
var schemaSQL string

// defaultRolePermissions is the seed mapping inserted with INSERT OR IGNORE, so
// operators can freely edit role_permissions afterwards without it being reset.
// Kept here only as a first-run default; the running app always reads the table.
var defaultRolePermissions = map[string][]string{
	// reporter: submit sightings and view canonical history.
	"reporter": {"sight", "view"},
	// editor: everything a reporter can do, plus reconcile / canonicalize / gossip.
	"editor": {"sight", "view", "reconcile", "canonicalize", "gossip"},
	// administrator: full set, including audit and flagging users insincere.
	"administrator": {
		"sight", "view", "reconcile", "canonicalize",
		"gossip", "audit", "flag_insincere", "set-user-flags",
	},
	// muted: may look but not contribute.
	"muted": {"view"},
}

// Open opens (creating if needed) the SQLite database at path, applies the
// schema (all CREATE TABLE ... IF NOT EXISTS), ensures the user-ID hashing seed
// exists, and seeds default permissions.
func Open(path string) (*sql.DB, error) {
	// _pragma busy_timeout avoids "database is locked" under concurrent writers;
	// foreign_keys must be enabled per-connection for the modernc driver.
	dsn := fmt.Sprintf("file:%s?_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)&_pragma=journal_mode(WAL)", path)
	sqlDB, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	if err := sqlDB.Ping(); err != nil {
		sqlDB.Close()
		return nil, fmt.Errorf("ping sqlite: %w", err)
	}
	if err := applySchema(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	seed, err := ensureUserIDHashSeed(sqlDB)
	if err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := dropUserIdentities(sqlDB, seed); err != nil {
		sqlDB.Close()
		return nil, err
	}
	if err := seedRolePermissions(sqlDB); err != nil {
		sqlDB.Close()
		return nil, err
	}
	return sqlDB, nil
}

// applySchema runs the embedded DDL. Every statement is idempotent
// (IF NOT EXISTS), so this is safe to run on every startup.
func applySchema(sqlDB *sql.DB) error {
	if _, err := sqlDB.Exec(schemaSQL); err != nil {
		return fmt.Errorf("apply schema: %w", err)
	}
	return nil
}

// ensureUserIDHashSeed returns the seed behind every users.opaque_id, creating
// it on first run. The INSERT-then-SELECT order means an existing seed always
// wins, so restarts never rotate it (which would orphan every user).
func ensureUserIDHashSeed(sqlDB *sql.DB) (string, error) {
	candidate, err := identity.NewSeed()
	if err != nil {
		return "", err
	}
	if _, err := sqlDB.Exec(
		`INSERT OR IGNORE INTO config (key, value, updated_at) VALUES (?, ?, ?)`,
		identity.SeedConfigKey, candidate, time.Now().UTC().Format(time.RFC3339),
	); err != nil {
		return "", fmt.Errorf("seed %s: %w", identity.SeedConfigKey, err)
	}
	var seed string
	if err := sqlDB.QueryRow(
		`SELECT value FROM config WHERE key = ?`, identity.SeedConfigKey,
	).Scan(&seed); err != nil {
		return "", fmt.Errorf("read %s: %w", identity.SeedConfigKey, err)
	}
	if seed == candidate {
		log.Printf("db: generated a new %s", identity.SeedConfigKey)
	}
	return seed, nil
}

// dropUserIdentities removes the legacy private subject table. Databases created
// before opaque IDs became derived have a real oauth2_proxy subject per user, so
// each user's opaque_id is first recomputed from it -- otherwise the existing
// rows (and their sightings, flags and notes) would be unreachable forever.
//
// Self-disabling: once the table is gone this is a no-op, and fresh databases
// never create it.
func dropUserIdentities(sqlDB *sql.DB, seed string) error {
	var name string
	err := sqlDB.QueryRow(
		`SELECT name FROM sqlite_master WHERE type = 'table' AND name = 'user_identities'`,
	).Scan(&name)
	if err == sql.ErrNoRows {
		return nil
	}
	if err != nil {
		return fmt.Errorf("look for user_identities: %w", err)
	}

	tx, err := sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()

	rows, err := tx.Query(`SELECT user_id, subject FROM user_identities`)
	if err != nil {
		return fmt.Errorf("read user_identities: %w", err)
	}
	type rehash struct {
		userID int64
		opaque string
	}
	var pending []rehash
	for rows.Next() {
		var userID int64
		var subject string
		if err := rows.Scan(&userID, &subject); err != nil {
			rows.Close()
			return err
		}
		opaque, err := identity.OpaqueUserID(seed, subject)
		if err != nil {
			rows.Close()
			return err
		}
		pending = append(pending, rehash{userID, opaque})
	}
	rows.Close()
	if err := rows.Err(); err != nil {
		return err
	}

	for _, p := range pending {
		if _, err := tx.Exec(
			`UPDATE users SET opaque_id = ? WHERE id = ?`, p.opaque, p.userID,
		); err != nil {
			return fmt.Errorf("rehash opaque_id for user %d: %w", p.userID, err)
		}
	}
	if _, err := tx.Exec(`DROP TABLE user_identities`); err != nil {
		return fmt.Errorf("drop user_identities: %w", err)
	}
	if err := tx.Commit(); err != nil {
		return err
	}
	log.Printf("db: dropped user_identities; rehashed %d user(s) to derived opaque IDs", len(pending))
	return nil
}

func seedRolePermissions(sqlDB *sql.DB) error {
	tx, err := sqlDB.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	for role, perms := range defaultRolePermissions {
		for _, perm := range perms {
			if _, err := tx.Exec(
				`INSERT OR IGNORE INTO role_permissions (role, permission) VALUES (?, ?)`,
				role, perm,
			); err != nil {
				return fmt.Errorf("seed role_permissions: %w", err)
			}
		}
	}
	return tx.Commit()
}
