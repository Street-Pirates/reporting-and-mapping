package store

import (
	"context"
	"database/sql"
	"time"
)

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
