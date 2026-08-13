package store

import (
	"context"
	"database/sql"
	"fmt"
	"regexp"
	"strings"
	"time"

	"sightingmap/internal/geo"
)

var titleDescription = regexp.MustCompile(`[A-Z][A-Z]* *- *(.*) *\(.*\)\s*`)

// var imageMarkup = regexp.MustCompile(`(?:<br/?>)*<img *src="(https://mymaps.usercontent.google.com/hostedimage/[^"]+)"(?: +\w+="[^"]*")* */?>(?:<br>)*(.*)`)
var imageMarkup = regexp.MustCompile(`<img *src="(https://mymaps.usercontent.google.com/hostedimage/[^"]+)"(?: +\w+="[^"]*")* */?>(?:<br>)*(.*)`)

// UnifyRadiusM is the max distance (metres, approximate/bounding-box) within
// which a new import record snaps onto a prior point of the same source —
// reusing that point's exact coordinates and canonical location so GPS/coord
// jitter across records collapses to a single shared point.
const UnifyRadiusM = 10.0

// ImportRecord is one normalized record from an external source (e.g. a KML
// placemark) to be reconciled into the append-only model.
type ImportRecord struct {
	// ExternalID is a stable natural key, namespaced "<source>:<hash>". The same
	// real-world record must produce the same ExternalID on every run so that a
	// re-import reconciles to the sightings it created before instead of
	// duplicating them.
	When        *time.Time
	ExternalID  string
	Lat, Lon    float64
	Name        string
	Description string
	// Exists is the state the source reports: true = the sign exists, false = it
	// no longer exists (plundered/removed).
	Exists bool
	// Message is a normalized message value for the imported sighting.
	Message string
	// Medium is the sighting medium for the imported record.
	Medium string
}

// ImportStats summarizes what an import run did — or, for a dry run, what it
// would have done.
type ImportStats struct {
	Records            int // records considered
	Inserted           int // corrective sightings appended
	Unchanged          int // records already reflecting the reported state
	CreatedCanonical   int // new canonical locations created
	AdoptedCanonical   int // pre-existing canonical locations reconciled to
	Unified            int // sightings snapped onto an existing point within ~10m
	NewExistence       int // of Inserted, how many of each report type
	ContinuedExistence int
	NonExistence       int
}

// ImportSourceRecords reconciles a batch of external-source records into the
// append-only model within a single transaction. For each record:
//
//   - It is matched to prior sightings by ExternalID (stable across runs), so a
//     record keeps the same canonical location on every subsequent import.
//   - A corrective sighting is appended only when the reported existence state
//     differs from what the source last told us — re-running an unchanged map is
//     a no-op. "Exists" becomes continued_existence, or new_existence the first
//     time we record the sign; "gone" becomes non_existence.
//   - When a record is new, it first tries to UNIFY onto a prior point of this
//     source lying within ~UnifyRadiusM (approximate, bounding box): a single
//     INSERT ... SELECT ... FROM sightings reuses that point's exact coordinates
//     and canonical location, so jittered coordinates for the same real-world
//     sign collapse onto one shared point.
//   - Failing that, a pre-existing canonical location that no record of this
//     source already owns and lies within ReconcileRadiusM is adopted
//     ("reconcile these with items already in the db"); otherwise a new canonical
//     location is created. Requiring the candidate to be unowned by this source
//     keeps distinct source records that are far apart from collapsing.
//
// sourceLike is a LIKE pattern for this source's external_ids (e.g.
// "pirate-map:%") and scopes the ownership test above. observedAt is stamped on
// every appended sighting. When dryRun is true the transaction is rolled back,
// so the returned stats describe what would happen without changing anything.
func (s *Store) ImportSourceRecords(ctx context.Context, importerUserID int64, sourceLike string, observedAt time.Time, recs []ImportRecord, dryRun bool) (ImportStats, error) {
	var stats ImportStats

	tx, err := s.db.Begin()
	if err != nil {
		return stats, err
	}
	defer tx.Rollback() // no-op after an explicit Commit; also discards a dry run

	observation_time := observedAt.UTC().Format(time.RFC3339)

	for _, rec := range recs {
		stats.Records++
		_, err := tx.ExecContext(ctx, `
			INSERT INTO locations (lat, lon, created_by, created_at)
			select $1, $2, $3, $4
			where not exists (
				select 1 from locations where lat between $1-0.000045 and $1+0.000045 and lon between $2-0.000045 and $2+0.000045
			)`, rec.Lat, rec.Lon, importerUserID, observedAt)

		if err != nil {
			return stats, fmt.Errorf("insert location %s: %w", rec.ExternalID, err)
		}

		var transpired string
		switch {
		case rec.Exists:
			transpired = "seen" // first time we record this sign
		default:
			transpired = "unknown" // reported gone
		}

		description := titleDescription.ReplaceAllString(rec.Description, "$1")
		var image_url *string
		if m := imageMarkup.FindStringSubmatch(description); len(m) > 1 {
			image_url = &m[1]
			description = imageMarkup.ReplaceAllString(description, "$2")
		}

		if strings.Contains(description, "<img") {
			//panic(description)
		}

		_, err = tx.ExecContext(ctx, `
			INSERT INTO sightings
			  (reporter_id, location_id, observed_at, transpired, image_url, medium, message, description, external_id)
			SELECT $1, loc.id, $2, $3, $4, $5, $6, $7, $8
			FROM locations loc
			WHERE lat BETWEEN $9-0.000045 AND $9+0.000045 AND lon BETWEEN $10-0.000045 AND $10+0.000045
			LIMIT 1`,
			importerUserID, observation_time, transpired, image_url, rec.Medium, rec.Message, description, rec.ExternalID,
			rec.Lat, rec.Lon,
		)
		if err != nil {
			return stats, fmt.Errorf("insert sighting %s: %w", rec.ExternalID, err)
		}
	}

	if dryRun {
		return stats, nil // deferred Rollback discards everything
	}
	if err := tx.Commit(); err != nil {
		return stats, err
	}
	return stats, nil
}

// adoptCanonicalTx returns the id of the nearest canonical location within
// radiusM metres of (lat,lon) that no sighting of this source already owns, or
// ok=false if there is none. Bounding-box prefilter in SQL, exact haversine in
// Go — the same pattern as CanonicalHistory's nearby-unreconciled query.
func adoptCanonicalTx(ctx context.Context, tx *sql.Tx, lat, lon, radiusM float64, sourceLike string) (int64, bool, error) {
	bb := geo.BoundingBox(lat, lon, radiusM)
	rows, err := tx.QueryContext(ctx, `
		SELECT c.id, c.lat, c.lon
		FROM canonical_locations c
		WHERE c.lat BETWEEN ? AND ? AND c.lon BETWEEN ? AND ?
		  AND NOT EXISTS (
		    SELECT 1 FROM sightings s
		    WHERE s.canonical_location_id = c.id AND s.external_id LIKE ?)`,
		bb.MinLat, bb.MaxLat, bb.MinLon, bb.MaxLon, sourceLike)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()

	var bestID int64
	bestDist := radiusM
	ok := false
	for rows.Next() {
		var id int64
		var clat, clon float64
		if err := rows.Scan(&id, &clat, &clon); err != nil {
			return 0, false, err
		}
		if d := geo.DistanceMeters(lat, lon, clat, clon); d <= bestDist {
			bestDist, bestID, ok = d, id, true
		}
	}
	return bestID, ok, rows.Err()
}

func insertCanonicalTx(tx *sql.Tx, userID int64, lat, lon float64, description, now string) (int64, error) {
	res, err := tx.Exec(`
		INSERT INTO canonical_locations (lat, lon, description, created_by, created_at)
		VALUES (?, ?, ?, ?, ?)`, lat, lon, description, userID, now)
	if err != nil {
		return 0, err
	}
	return res.LastInsertId()
}

// insertSightingTx appends one corrective sighting, already reconciled to
// canonicalID, at the given coordinates.
func insertSightingTx(tx *sql.Tx, userID int64, lat, lon float64, obs, now, reportType, description string, canonicalID int64, externalID string) error {
	_, err := tx.Exec(`
		INSERT INTO sightings
		  (reporter_id, lat, lon, observed_at, created_at, report_type, description, canonical_location_id, external_id)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		userID, lat, lon, obs, now, reportType, description, canonicalID, externalID)
	return err
}
