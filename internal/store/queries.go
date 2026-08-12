package store

import (
	"context"
	"database/sql"
	"strings"
	"time"

	"sightingmap/internal/geo"
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
		out = append(out, s)
	}
	return out, rows.Err()
}

func boolToInt(b bool) int {
	if b {
		return 1
	}
	return 0
}

// bboxClause returns a SQL fragment constraining alias.lat/alias.lon to bbox,
// prefixed by connector ("WHERE" for a query with no existing predicate, "AND"
// to extend one), together with its positional args. When bbox is nil it
// returns an empty fragment and no args, so the query is unfiltered.
func bboxClause(alias, connector string, bbox *geo.BBox) (string, []any) {
	if bbox == nil {
		return "", nil
	}
	frag := " " + connector + " " + alias + ".lat BETWEEN ? AND ? AND " + alias + ".lon BETWEEN ? AND ?"
	return frag, []any{bbox.MinLat, bbox.MaxLat, bbox.MinLon, bbox.MaxLon}
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

// AuditEventsSince returns interleaved sightings and location creation events.
func (s *Store) AuditEventsSince(ctx context.Context, since time.Time) ([]AuditEvent, error) {
	sinceStr := since.UTC().Format(time.RFC3339)
	rows, err := s.db.QueryContext(ctx, `
        SELECT 'location_created' AS event_type,
               l.id AS event_id,
               u.opaque_id AS author_opaque_id,
			   coalesce((select sum(uf.value) from user_flags uf where uf.target_user_id=u.id and uf.flag_type='insincere'), 0) as insincerity,
               l.lat AS lat,
               l.lon AS lon,
               l.created_at AS timestamp,
               NULL AS to_exist,
               NULL AS description,
               NULL AS medium,
               NULL AS message,
               NULL AS height,
               NULL AS location_id
          FROM locations l
          JOIN users u ON u.id = l.created_by
         WHERE l.created_at >= $1
        UNION ALL
        SELECT 'sighting' AS event_type,
               s.id AS event_id,
               u.opaque_id AS author_opaque_id,
			   coalesce((select sum(uf.value) from user_flags uf where uf.target_user_id=u.id and uf.flag_type='insincere'), 0) as insincerity,
               l.lat AS lat,
               l.lon AS lon,
               s.observed_at AS timestamp,
               s.to_exist AS to_exist,
               s.description AS description,
               s.medium AS medium,
               s.message AS message,
               s.height AS height,
               s.location_id AS location_id
          FROM sightings s
		  join locations l ON l.id = s.location_id
          JOIN users u ON u.id = s.reporter_id
         WHERE s.observed_at >= $1
         ORDER BY timestamp DESC, event_type DESC, event_id DESC
		 LIMIT 500`, sinceStr)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	var out []AuditEvent = []AuditEvent{}
	for rows.Next() {
		var ev AuditEvent
		var toExist sql.NullInt64
		if err := rows.Scan(&ev.EventType, &ev.EventID, &ev.AuthorOpaqueID, &ev.AuthorInsincerity, &ev.Lat, &ev.Lon, &ev.Timestamp, &toExist, &ev.Description, &ev.Medium, &ev.Message, &ev.Height, &ev.LocationID); err != nil {
			return nil, err
		}
		if toExist.Valid {
			t := toExist.Int64 != 0
			ev.ToExist = &t
		}
		out = append(out, ev)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

func (s *Store) LocationHistory(ctx context.Context, locationID int64, since time.Time, includeInsincere bool) (*LocationHistory, error) {
	loc, err := s.getLocation(locationID)
	if err != nil {
		return nil, err
	}
	sinceStr := since.UTC().Format(time.RFC3339)
	inc := boolToInt(includeInsincere)

	recRows, err := s.db.QueryContext(ctx, sightingSelect+`
        WHERE s.location_id = ?
        AND s.observed_at >= ?
        AND ((NOT ?) OR NOT EXISTS (SELECT 1 FROM user_flags uf WHERE uf.target_user_id = s.reporter_id AND uf.flag_type = 'insincere'))
        ORDER BY s.observed_at ASC, s.id DESC`,
		locationID, sinceStr, inc)
	if err != nil {
		return nil, err
	}
	reconciled, err := scanSightings(recRows)
	if err != nil {
		return nil, err
	}

	// Nearby unreconciled: bounding-box prefilter in SQL, exact haversine in Go.
	bb := geo.BoundingBox(loc.Lat, loc.Lon, ReconcileRadiusM)
	nearRows, err := s.db.Query(sightingSelect+`
        WHERE s.location_id IS NULL
          AND l.lat BETWEEN ? AND ? AND l.lon BETWEEN ? AND ?
          AND s.observed_at >= ?
          AND (? = 1 OR s.reporter_id AND NOT EXISTS (SELECT 1 FROM user_flags uf WHERE uf.target_user_id = s.reporter_id AND uf.flag_type = 'insincere'))
        ORDER BY s.observed_at DESC, s.id DESC`,
		bb.MinLat, bb.MaxLat, bb.MinLon, bb.MaxLon, sinceStr, inc)
	if err != nil {
		return nil, err
	}
	candidates, err := scanSightings(nearRows)
	if err != nil {
		return nil, err
	}
	nearby := make([]Sighting, 0, len(candidates))
	for _, c := range candidates {
		d := geo.DistanceMeters(loc.Lat, loc.Lon, c.Lat, c.Lon)
		if d <= ReconcileRadiusM {
			dd := d
			c.DistanceM = &dd
			nearby = append(nearby, c)
		}
	}

	return &LocationHistory{Location: loc, Reconciled: reconciled, NearbyUnrec: nearby}, nil
}

// MapData returns canonical markers (with derived current status) plus
// unreconciled new-existence proposals, filtered to observed_at >= since. When
// bbox is non-nil, markers are further limited to those whose coordinates fall
// inside the box (the client's expanded viewport). By default only markers
// whose latest report is "present" are returned; includeMissing also returns
// reported-missing (latest non_existence) markers.
func (s *Store) MapData(ctx context.Context, since time.Time, includeInsincere, includeMissing bool, bbox *geo.BBox) (*MapData, error) {
	sinceStr := since.UTC().Format(time.RFC3339)
	out := &MapData{Markers: []MapMarker{}, Proposed: []Sighting{}}

	qu := `with loc as (
		select id, lat, lon, created_by from locations where lat between $1 and $2 and lon between $3 and $4
	)
	select 
		sightings.id sighting_id, to_exist = 1 as does_exist, max(observed_at) as observed_at, sightings.image_url, sightings.description, count(sightings.id),
		sightings.medium, sightings.message, sightings.height,
		loc.id location_id, loc.lat lat, loc.lon lon 
	from loc
	join sightings on sightings.location_id = loc.id
	where observed_at >= $5
	and ($6 or coalesce((select sum(uf.value) from user_flags uf where uf.target_user_id = sightings.reporter_id and uf.flag_type = 'insincere'), 0) <= 0)
	group by loc.id
	having ($7 or (to_exist = 1))
	order by observed_at desc, loc.id
	`

	rows, err := s.db.QueryContext(ctx, qu, bbox.MinLat, bbox.MaxLat, bbox.MinLon, bbox.MaxLon, sinceStr, includeInsincere, includeMissing)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var m MapMarker
		if err := rows.Scan(&m.ID, &m.Exists, &m.LatestObservedAt, &m.ImageURL, &m.Description, &m.SightingCount,
			&m.Medium, &m.Message, &m.Height,
			&m.Location.ID, &m.Lat, &m.Lon); err != nil {
			return nil, err
		}
		m.Title = titleFromSignTraits(m.Message, m.Medium, m.Height)

		out.Markers = append(out.Markers, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}

func titleFromSignTraits(message, medium, height string) string {
	msg := strings.Builder{}
	if message != "unknown" && message != "other" && message != "" {
		msg.WriteString(message)
	}

	if medium != "unknown" && medium != "" {
		if msg.Len() > 0 {
			msg.WriteString(" ")
		}
		msg.WriteString(medium)
	}

	if height != "unknown" && height != "" {
		if msg.Len() > 0 {
			msg.WriteString(" ")
		}
		msg.WriteString("at height ")
		msg.WriteString(height)
	}

	if msg.Len() == 0 {
		msg.WriteString("mysterious!")
	}

	return msg.String()
}
