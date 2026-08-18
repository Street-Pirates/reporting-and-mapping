package store

import (
	"context"
	"sightingmap/internal/geo"
	"time"

	olc "github.com/google/open-location-code/go"
)

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
		sightings.id sighting_id, (transpired = 'seen' OR to_exist = 1) as does_exist, max(observed_at) as observed_at, sightings.image_url, sightings.description, count(sightings.id),
		sightings.medium, sightings.message, sightings.height,
		loc.id location_id, loc.lat lat, loc.lon lon 
	from loc
	join sightings on sightings.location_id = loc.id
	where observed_at >= $5
	and ($6 or coalesce((select sum(uf.value) from user_flags uf where uf.target_user_id = sightings.reporter_id and uf.flag_type = 'insincere'), 0) <= 0)
	group by loc.id
	having ($7 or (transpired = 'seen' or to_exist = 1))
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

		m.Location.PlusCode = olc.Encode(m.Location.Lat, m.Location.Lon, 11)

		out.Markers = append(out.Markers, m)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}

	return out, nil
}
