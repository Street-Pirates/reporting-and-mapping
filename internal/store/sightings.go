package store

import (
	"context"
	"database/sql"
	"time"

	olc "github.com/google/open-location-code/go"
)

// sightingSelect is the common column list + join used everywhere a Sighting is
// read, so the opaque author id (never the internal user id) is always exposed.
const sightingSelect = `
	SELECT s.id, u.opaque_id, l.lat, l.lon, s.observed_at,
	       s.to_exist, s.description, s.medium, s.message, s.height, s.location_id
	FROM sightings s
	LEFT JOIN users u ON u.id = s.reporter_id
	LEFT JOIN locations l ON l.id = s.location_id`

func scanSightings(rows *sql.Rows) ([]Sighting, error) {
	defer rows.Close()
	var out []Sighting
	for rows.Next() {
		var s Sighting
		if err := rows.Scan(
			&s.ID, &s.AuthorOpaqueID, &s.Lat, &s.Lon, &s.ObservedAt,
			&s.ToExist, &s.Description, &s.Medium, &s.Message, &s.Height, &s.LocationID,
		); err != nil {
			return nil, err
		}
		s.Location.PlusCode = olc.Encode(s.Location.Lat, s.Location.Lon, 11)
		out = append(out, s)
	}
	return out, rows.Err()
}

// SightingsByOpaqueUser returns every sighting by the given opaque user id
// (endpoint #5, gated by `audit`). Insincere flagging does not hide data here —
// audit deliberately sees everything.
func (s *Store) SightingsByOpaqueUser(ctx context.Context, opaqueID string) ([]Sighting, error) {
	uid, err := s.userByOpaque(ctx, opaqueID)
	if err != nil {
		return nil, err
	}
	rows, err := s.db.QueryContext(ctx, sightingSelect+`
		WHERE s.reporter_id = ?
		ORDER BY s.observed_at DESC, s.id DESC`, uid)
	if err != nil {
		return nil, err
	}
	return scanSightings(rows)
}

// SightingsSince returns all sightings observed at or after the provided time,
// ordered newest-first. Used by the audit UI to inspect recent submissions.
func (s *Store) SightingsSince(ctx context.Context, since time.Time) ([]Sighting, error) {
	sinceStr := since.UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, sightingSelect+`
		WHERE s.observed_at >= ?
		ORDER BY s.observed_at DESC, s.id DESC`, sinceStr)
	if err != nil {
		return nil, err
	}
	return scanSightings(rows)
}
