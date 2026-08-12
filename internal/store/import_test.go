package store

import (
	"path/filepath"
	"testing"
	"time"

	"sightingmap/internal/db"
)

func newTestStore(t *testing.T) (*Store, int64) {
	t.Helper()
	sqlDB, err := db.Open(filepath.Join(t.TempDir(), "test.db"))
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	t.Cleanup(func() { sqlDB.Close() })
	st := New(sqlDB)
	u, err := st.ResolveUser("import:test")
	if err != nil {
		t.Fatalf("resolve importer: %v", err)
	}
	return st, u.ID
}

// latestType returns the most recent report_type recorded for an external_id.
func latestType(t *testing.T, st *Store, ext string) string {
	t.Helper()
	var rt string
	err := st.db.QueryRowContext(t.Context(), `
		SELECT report_type FROM sightings WHERE external_id = ?
		ORDER BY observed_at DESC, id DESC LIMIT 1`, ext).Scan(&rt)
	if err != nil {
		t.Fatalf("latestType(%s): %v", ext, err)
	}
	return rt
}

func count(t *testing.T, st *Store, q string) int {
	t.Helper()
	var n int
	if err := st.db.QueryRowContext(t.Context(), q).Scan(&n); err != nil {
		t.Fatalf("count %q: %v", q, err)
	}
	return n
}

func TestImportSourceRecords_LifecycleAndIdempotency(t *testing.T) {
	st, uid := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const like = "test:%"

	exists := ImportRecord{ExternalID: "test:a", Lat: 34.0, Lon: -117.0, Description: "A", Exists: true}
	gone := ImportRecord{ExternalID: "test:b", Lat: 35.0, Lon: -118.0, Description: "B", Exists: false}

	// First import: one existence report, one gone report; two new canonicals.
	s, err := st.ImportSourceRecords(t.Context(), uid, like, t0, []ImportRecord{exists, gone}, false)
	if err != nil {
		t.Fatalf("import 1: %v", err)
	}
	if s.Inserted != 2 || s.NewExistence != 1 || s.NonExistence != 1 || s.CreatedCanonical != 2 || s.Unchanged != 0 {
		t.Fatalf("import 1 stats = %+v", s)
	}
	if got := latestType(t, st, "test:a"); got != ReportNewExistence {
		t.Errorf("a latest = %q, want new_existence", got)
	}
	if got := latestType(t, st, "test:b"); got != ReportNonExistence {
		t.Errorf("b latest = %q, want non_existence", got)
	}

	// Re-import identical data: fully idempotent (no rows written).
	s, err = st.ImportSourceRecords(t.Context(), uid, like, t0, []ImportRecord{exists, gone}, false)
	if err != nil {
		t.Fatalf("import 2: %v", err)
	}
	if s.Inserted != 0 || s.Unchanged != 2 || s.CreatedCanonical != 0 {
		t.Fatalf("import 2 (idempotent) stats = %+v", s)
	}
	if n := count(t, st, "SELECT count(*) FROM sightings"); n != 2 {
		t.Fatalf("sightings after idempotent re-run = %d, want 2", n)
	}

	// Flip both states: A is now gone, B is back. Append corrections; reuse
	// the existing canonical locations (nothing created).
	exists.Exists = false
	gone.Exists = true
	s, err = st.ImportSourceRecords(t.Context(), uid, like, t0.Add(time.Hour), []ImportRecord{exists, gone}, false)
	if err != nil {
		t.Fatalf("import 3: %v", err)
	}
	if s.Inserted != 2 || s.NonExistence != 1 || s.ContinuedExistence != 1 || s.CreatedCanonical != 0 {
		t.Fatalf("import 3 (flip) stats = %+v", s)
	}
	if got := latestType(t, st, "test:a"); got != ReportNonExistence {
		t.Errorf("a after flip = %q, want non_existence", got)
	}
	if got := latestType(t, st, "test:b"); got != ReportContinuedExistence {
		t.Errorf("b after flip = %q, want continued_existence", got)
	}
	if n := count(t, st, "SELECT count(DISTINCT canonical_location_id) FROM sightings"); n != 2 {
		t.Fatalf("distinct canonical locations = %d, want 2 (reused, not duplicated)", n)
	}

	// The derived map reflects the flip: A gone (exists=false), B back (exists=true).
	md, err := st.MapData(t.Context(), t0.Add(-24*time.Hour), false, true, nil)
	if err != nil {
		t.Fatalf("MapData: %v", err)
	}
	for _, m := range md.Markers {
		switch {
		case m.Lat == 34.0 && m.Exists:
			t.Errorf("marker A should be gone, got exists=true")
		case m.Lat == 35.0 && !m.Exists:
			t.Errorf("marker B should exist, got exists=false")
		}
	}
}

func TestImportSourceRecords_SpatialAdoptionAndNoMerge(t *testing.T) {
	st, uid := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const like = "test:%"

	// A pre-existing (non-source) canonical location.
	preID, err := st.InsertCanonical(t.Context(), uid, 40.0, -74.0)
	if err != nil {
		t.Fatalf("seed canonical: %v", err)
	}

	// A record ~55m away should ADOPT the pre-existing canonical, not create one.
	near := ImportRecord{ExternalID: "test:near", Lat: 40.0005, Lon: -74.0, Exists: true}
	s, err := st.ImportSourceRecords(t.Context(), uid, like, t0, []ImportRecord{near}, false)
	if err != nil {
		t.Fatalf("import near: %v", err)
	}
	if s.AdoptedCanonical != 1 || s.CreatedCanonical != 0 {
		t.Fatalf("adoption stats = %+v, want adopted 1 created 0", s)
	}
	var adopted int64
	if err := st.db.QueryRowContext(
		t.Context(), `SELECT canonical_location_id FROM sightings WHERE external_id = 'test:near'`).Scan(&adopted); err != nil {
		t.Fatal(err)
	}
	if adopted != preID {
		t.Errorf("adopted canonical = %d, want pre-existing %d", adopted, preID)
	}

	// Two distinct records ~33m apart (far from preID) must NOT merge: each gets
	// its own canonical, because the first one's canonical becomes source-owned.
	a := ImportRecord{ExternalID: "test:a", Lat: 10.0, Lon: 20.0, Exists: true}
	b := ImportRecord{ExternalID: "test:b", Lat: 10.0003, Lon: 20.0, Exists: true}
	s, err = st.ImportSourceRecords(t.Context(), uid, like, t0, []ImportRecord{a, b}, false)
	if err != nil {
		t.Fatalf("import pair: %v", err)
	}
	if s.CreatedCanonical != 2 || s.AdoptedCanonical != 0 {
		t.Fatalf("no-merge stats = %+v, want created 2 adopted 0", s)
	}
}

func TestImportSourceRecords_UnifiesNearbyJitterToOnePoint(t *testing.T) {
	st, uid := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	const like = "test:%"

	// First record establishes the point + canonical location.
	a := ImportRecord{ExternalID: "test:a", Lat: 10.0, Lon: 20.0, Exists: true}
	// Second record, a DIFFERENT sign, ~5.5m north (0.00005 deg lat) — GPS jitter
	// for the same real-world point. It must snap onto A: same coords, same canonical.
	b := ImportRecord{ExternalID: "test:b", Lat: 10.00005, Lon: 20.0, Exists: true}

	s, err := st.ImportSourceRecords(t.Context(), uid, like, t0, []ImportRecord{a, b}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if s.CreatedCanonical != 1 || s.Unified != 1 || s.Inserted != 2 {
		t.Fatalf("stats = %+v, want created 1, unified 1, inserted 2", s)
	}
	if n := count(t, st, "SELECT count(*) FROM canonical_locations"); n != 1 {
		t.Errorf("canonical locations = %d, want 1 (unified)", n)
	}
	if n := count(t, st, "SELECT count(DISTINCT canonical_location_id) FROM sightings"); n != 1 {
		t.Errorf("distinct canonical ids on sightings = %d, want 1", n)
	}
	// B's stored coordinates must equal A's exactly (snapped, not the jittered input).
	var blat, blon float64
	if err := st.db.QueryRowContext(
		t.Context(), `SELECT lat, lon FROM sightings WHERE external_id = 'test:b'`).Scan(&blat, &blon); err != nil {
		t.Fatal(err)
	}
	if blat != 10.0 || blon != 20.0 {
		t.Errorf("B stored at (%v,%v), want snapped to A (10,20)", blat, blon)
	}
}

func TestImportSourceRecords_PersistsMessage(t *testing.T) {
	st, uid := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := []ImportRecord{{ExternalID: "test:msg", Lat: 1, Lon: 1, Exists: true, Message: "js", Medium: "placard"}}

	if _, err := st.ImportSourceRecords(t.Context(), uid, "test:%", t0, recs, false); err != nil {
		t.Fatalf("import: %v", err)
	}

	var gotMessage, gotMedium string
	if err := st.db.QueryRowContext(t.Context(), `SELECT message, medium FROM sightings WHERE external_id = ?`, "test:msg").Scan(&gotMessage, &gotMedium); err != nil {
		t.Fatalf("query message/medium: %v", err)
	}
	if gotMessage != "js" {
		t.Fatalf("stored message = %q, want %q", gotMessage, "js")
	}
	if gotMedium != "placard" {
		t.Fatalf("stored medium = %q, want %q", gotMedium, "placard")
	}
}

func TestImportSourceRecords_DryRunWritesNothing(t *testing.T) {
	st, uid := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	recs := []ImportRecord{
		{ExternalID: "test:a", Lat: 1, Lon: 1, Exists: true},
		{ExternalID: "test:b", Lat: 2, Lon: 2, Exists: false},
	}
	s, err := st.ImportSourceRecords(t.Context(), uid, "test:%", t0, recs, true)
	if err != nil {
		t.Fatalf("dry run: %v", err)
	}
	if s.Inserted != 2 || s.CreatedCanonical != 2 {
		t.Fatalf("dry-run stats = %+v, want it to report 2 inserts", s)
	}
	if n := count(t, st, "SELECT count(*) FROM sightings"); n != 0 {
		t.Errorf("dry run wrote %d sightings, want 0", n)
	}
	if n := count(t, st, "SELECT count(*) FROM canonical_locations"); n != 0 {
		t.Errorf("dry run wrote %d canonical locations, want 0", n)
	}
}

func TestImportSourceRecords_InBatchDuplicateDeduped(t *testing.T) {
	st, uid := newTestStore(t)
	t0 := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	dup := ImportRecord{ExternalID: "test:dup", Lat: 5, Lon: 5, Exists: false}
	s, err := st.ImportSourceRecords(t.Context(), uid, "test:%", t0, []ImportRecord{dup, dup}, false)
	if err != nil {
		t.Fatalf("import: %v", err)
	}
	if s.Inserted != 1 || s.Unchanged != 1 || s.CreatedCanonical != 1 {
		t.Fatalf("in-batch dup stats = %+v, want inserted 1 unchanged 1 created 1", s)
	}
}
