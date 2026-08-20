package store

import (
	"context"
	"time"

	"sightingmap/internal/geo"
)

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
			c.Location.PlusCode = geo.GetPlusCode(c.Location.Lat, c.Location.Lon)
			nearby = append(nearby, c)
		}
	}

	return &LocationHistory{Location: loc, Reconciled: reconciled, NearbyUnrec: nearby}, nil
}
