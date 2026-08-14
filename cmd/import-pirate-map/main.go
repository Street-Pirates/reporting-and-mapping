// Command import-pirate-map imports placemarks from the "Pirate Map" Google
// My Maps KML into the Ad-Sighting Tracker datastore.
//
// It fetches the map's KML export, then reconciles each placemark into the
// append-only model according to the folder ("group") it lives in:
//
//   - Folders named "Billboards" or "Sightings of Questionable Legality" are
//     discarded entirely.
//   - Folders whose name contains "Reported" are treated as reported-to-exist.
//   - Folders whose name contains "Plunder(ed/s)" or "[Rr]emoved" are treated
//     as reported-not-to-exist.
//
// Each placemark's natural key (a hash of its name + rounded coordinates, since
// this export carries no stable placemark id) is stored on the sighting as its
// external_id, so re-running the import reconciles to the same records instead
// of duplicating them. See internal/store.ImportSourceRecords for the reconcile
// and idempotency rules.
//
// Usage:
//
//	go run ./cmd/import-pirate-map                 # fetch + import into $DB_PATH
//	go run ./cmd/import-pirate-map -dry-run        # report what would change
//	go run ./cmd/import-pirate-map -file map.kml   # import from a local file
package main

import (
	"archive/zip"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/xml"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"os"
	"path"
	"regexp"
	"strconv"
	"strings"
	"time"

	"sightingmap/internal/db"
	"sightingmap/internal/store"
)

// defaultMID is the "Pirate Map" Google My Maps map id.
const defaultMID = "1RzIEJyke2uzahFOJh1vXA2Lh6mXl5jA"

const userAgent = "sightingmap-import/1.0 (+https://github.com/; pirate-map importer)"

// Folder classifications.
const (
	classExists  = "exists"  // "Reported ..." -> reported to exist
	classGone    = "gone"    // "...Plunder..." / "...removed..." -> reported gone
	classDiscard = "discard" // Billboards / Sightings of Questionable Legality
	classUnknown = "unknown" // anything unrecognized -> skipped with a warning
)

// var htmlLineBreak = regexp.MustCompile(`(?i)<br\s*/?>`)
var dateMmddyyMatcher = regexp.MustCompile(`([01][0-9]/[0123][0-9]/[0-9][0-9])`)

//var removedMatcher = regexp.MustCompile(`(?i)plunder|removed|gone`)

func main() {
	log.SetFlags(0)

	var (
		dbPath   = flag.String("db", env("DB_PATH", "sightingmap.db"), "SQLite database path")
		mid      = flag.String("mid", defaultMID, "Google My Maps map id (mid=)")
		url      = flag.String("url", "", "override the full KML URL (ignores -mid)")
		file     = flag.String("file", "", "read KML/KMZ from a local file instead of fetching")
		source   = flag.String("source", "pirate-map", "namespace prefix for external_ids")
		importer = flag.String("importer", "import:pirate-map", "importer identity (oauth2 subject)")
		dryRun   = flag.Bool("dry-run", false, "parse and report without writing sightings (still registers the importer identity)")
		timeout  = flag.Duration("timeout", 2*time.Minute, "HTTP fetch timeout")
	)
	flag.Parse()

	// 1. Obtain the KML bytes (local file or fetched export), unwrapping KMZ.
	raw, srcDesc, err := loadRaw(*file, *url, *mid, *timeout)
	if err != nil {
		log.Fatalf("load KML: %v", err)
	}
	kmlBytes, err := unwrapKMZ(raw)
	if err != nil {
		log.Fatalf("read KML: %v", err)
	}
	var doc kmlDoc
	if err := xml.Unmarshal(kmlBytes, &doc); err != nil {
		log.Fatalf("parse KML: %v", err)
	}

	// 2. Classify folders and build normalized import records.
	importDate := time.Date(2026, 6, 20, 22, 22, 0, 0, time.UTC) // most recent solstice
	recs, summary := buildRecords(doc, *source, importDate)
	log.Printf("Map %q from %s", doc.Document.Name, srcDesc)
	summary.printFolders()
	if len(recs) == 0 {
		log.Fatalf("no importable placemarks found")
	}

	// 3. Open the datastore and resolve the importer identity.
	sqlDB, err := db.Open(*dbPath)
	if err != nil {
		log.Fatalf("open db %s: %v", *dbPath, err)
	}
	defer sqlDB.Close()
	st := store.New(sqlDB)
	user, err := st.ResolveUser(*importer)
	if err != nil {
		log.Fatalf("resolve importer %q: %v", *importer, err)
	}

	// 4. Reconcile the batch (one transaction; rolled back on -dry-run).
	stats, err := st.ImportSourceRecords(context.Background(), user.ID, *source+":%", importDate, recs, *dryRun)
	if err != nil {
		log.Fatalf("import: %v", err)
	}

	// 5. Report.
	printSummary(*dbPath, *source, *dryRun, summary, stats)
}

// ---------------------------------------------------------------------------
// KML model & parsing
// ---------------------------------------------------------------------------

// Struct tags match KML local element names; the default kml namespace is
// ignored by encoding/xml when no namespace prefix is given. Descriptions are
// deliberately not parsed (they are large HTML blobs we do not need).
type kmlDoc struct {
	Document struct {
		Name       string      `xml:"name"`
		Folders    []kmlFolder `xml:"Folder"`
		Placemarks []kmlPlace  `xml:"Placemark"` // direct children (defensive)
	} `xml:"Document"`
}

type kmlFolder struct {
	Name       string     `xml:"name"`
	Placemarks []kmlPlace `xml:"Placemark"`
}

type kmlPlace struct {
	Name        string `xml:"name"`
	Description string `xml:"description"`
	Coordinates string `xml:"Point>coordinates"`
}

// buildRecords classifies every folder and turns retained placemarks into
// store.ImportRecords, returning them alongside a human-readable summary.
func buildRecords(doc kmlDoc, source string, defaultWhen time.Time) ([]store.ImportRecord, *runSummary) {
	sum := &runSummary{}
	var recs []store.ImportRecord

	add := func(folderName, cls string, pms []kmlPlace) {
		sum.folders = append(sum.folders, folderStat{name: folderName, class: cls, count: len(pms)})
		switch cls {
		case classDiscard:
			sum.discarded += len(pms)
			return
		case classUnknown:
			sum.discarded += len(pms)
			log.Printf("WARNING: folder %q did not match any rule; skipping its %d placemarks", folderName, len(pms))
			return
		}
		for _, pm := range pms {
			lat, lon, err := parseLonLat(pm.Coordinates)
			if err != nil || !validLatLon(lat, lon) {
				sum.badCoords++
				continue
			}
			desc := pm.Description

			recs = append(recs, store.ImportRecord{
				ExternalID:  naturalID(source, pm.Name, lat, lon),
				Lat:         lat,
				Lon:         lon,
				Name:        pm.Name,
				Description: desc,
				Exists:      cls == classExists,
				Message:     messageForFolder(folderName, desc),
				Medium:      mediumForDescription(desc),
			})
		}
	}

	for _, f := range doc.Document.Folders {
		add(f.Name, classify(f.Name), f.Placemarks)
	}
	// Placemarks directly under <Document> belong to no group; we cannot classify
	// them, so treat them like an unknown folder.
	if len(doc.Document.Placemarks) > 0 {
		add("(placemarks with no folder)", classUnknown, doc.Document.Placemarks)
	}
	return recs, sum
}

// classify maps a folder ("group") name to how its placemarks are reported.
// Discard rules win over the reported/gone keyword rules.
func classify(folderName string) string {
	n := strings.ToLower(folderName)
	switch {
	case strings.Contains(n, "billboard"), strings.Contains(n, "questionable legality"):
		return classDiscard
	case strings.Contains(n, "plunder"), strings.Contains(n, "removed"):
		return classGone
	case strings.Contains(n, "reported"):
		return classExists
	default:
		return classUnknown
	}
}

// messageForFolder derives a normalized sighting message from the folder name
// and, when the folder doesn't say, from the placemark description.
func messageForFolder(folderName, description string) string {
	n := strings.ToLower(folderName)
	switch {
	case strings.Contains(n, "jicr"):
		return "jicr"
	case strings.Contains(n, "js"):
		return "js"
	default:
		return messageFromDescription(description)
	}
}

func messageFromDescription(description string) string {
	d := strings.ToLower(description)
	switch {
	case strings.Contains(d, "jesus is coming"):
		return "jicr"
	case strings.Contains(d, "jesus saves"):
		return "js"
	case strings.Contains(d, "believe in jesus"):
		return "bij"
	default:
		return "unknown"
	}
}

func mediumForDescription(description string) string {
	if strings.Contains(strings.ToLower(description), "sticker") {
		return "sticker"
	}
	if strings.Contains(strings.ToLower(description), "barnacle") {
		return "sticker"
	}
	return "placard"
}

// naturalID derives a stable per-record key. This KML export carries no
// placemark id, so we hash the (near-unique) name + coordinates. Coordinates are
// rounded to ~0.1m so re-exports with different trailing precision stay stable
// while still distinguishing distinct signs.
func naturalID(source, name string, lat, lon float64) string {
	h := sha256.Sum256([]byte(name + "\n" + fmt.Sprintf("%.6f,%.6f", lat, lon)))
	return source + ":" + hex.EncodeToString(h[:8])
}

// parseLonLat reads a KML <coordinates> body ("lon,lat[,alt]", possibly
// whitespace-wrapped or multi-tuple) and returns the first tuple's lat/lon.
func parseLonLat(s string) (lat, lon float64, err error) {
	fields := strings.Fields(s) // collapses the indentation/newlines My Maps emits
	if len(fields) == 0 {
		return 0, 0, fmt.Errorf("empty coordinates")
	}
	parts := strings.Split(fields[0], ",")
	if len(parts) < 2 {
		return 0, 0, fmt.Errorf("bad coordinate tuple %q", fields[0])
	}
	if lon, err = strconv.ParseFloat(parts[0], 64); err != nil {
		return 0, 0, fmt.Errorf("bad lon %q: %w", parts[0], err)
	}
	if lat, err = strconv.ParseFloat(parts[1], 64); err != nil {
		return 0, 0, fmt.Errorf("bad lat %q: %w", parts[1], err)
	}
	return lat, lon, nil
}

func validLatLon(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// ---------------------------------------------------------------------------
// Fetching
// ---------------------------------------------------------------------------

// loadRaw returns the raw export bytes plus a short description of where they
// came from. A local file wins; otherwise it fetches the KML export.
func loadRaw(file, url, mid string, timeout time.Duration) ([]byte, string, error) {
	if file != "" {
		b, err := os.ReadFile(file)
		return b, "file " + file, err
	}
	if url == "" {
		// forcekml=1 asks for raw KML rather than a KMZ; we still handle KMZ below.
		url = fmt.Sprintf("https://www.google.com/maps/d/kml?mid=%s&forcekml=1", mid)
	}
	b, err := fetchURL(url, timeout)
	return b, url, err
}

func fetchURL(url string, timeout time.Duration) ([]byte, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("User-Agent", userAgent)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		return nil, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return nil, fmt.Errorf("GET %s: %s", url, resp.Status)
	}
	return io.ReadAll(resp.Body)
}

// unwrapKMZ returns KML bytes, transparently extracting the .kml entry when the
// input is a KMZ (zip) archive.
func unwrapKMZ(raw []byte) ([]byte, error) {
	if !bytes.HasPrefix(raw, []byte("PK\x03\x04")) {
		return raw, nil // already plain KML/XML
	}
	zr, err := zip.NewReader(bytes.NewReader(raw), int64(len(raw)))
	if err != nil {
		return nil, fmt.Errorf("open KMZ: %w", err)
	}
	for _, f := range zr.File {
		if strings.EqualFold(path.Base(f.Name), "doc.kml") || strings.HasSuffix(strings.ToLower(f.Name), ".kml") {
			rc, err := f.Open()
			if err != nil {
				return nil, err
			}
			defer rc.Close()
			return io.ReadAll(rc)
		}
	}
	return nil, fmt.Errorf("KMZ archive contains no .kml entry")
}

// ---------------------------------------------------------------------------
// Reporting
// ---------------------------------------------------------------------------

type folderStat struct {
	name  string
	class string
	count int
}

type runSummary struct {
	folders   []folderStat
	discarded int // placemarks in discarded/unknown folders
	badCoords int // placemarks skipped for unusable coordinates
}

func (s *runSummary) printFolders() {
	for _, f := range s.folders {
		log.Printf("  %-8s %5d  %q", f.class, f.count, f.name)
	}
}

func printSummary(dbPath, source string, dryRun bool, sum *runSummary, st store.ImportStats) {
	mode := ""
	if dryRun {
		mode = "  [DRY RUN — nothing written]"
	}
	log.Printf("")
	log.Printf("Import summary (db=%s, source=%s)%s", dbPath, source, mode)
	log.Printf("  discarded placemarks:      %d", sum.discarded)
	if sum.badCoords > 0 {
		log.Printf("  skipped (bad coordinates): %d", sum.badCoords)
	}
	log.Printf("  records considered:        %d", st.Records)
	log.Printf("  sightings appended:        %d  (new_existence %d, continued_existence %d, non_existence %d)",
		st.Inserted, st.NewExistence, st.ContinuedExistence, st.NonExistence)
	log.Printf("  unchanged (skipped):       %d", st.Unchanged)
	log.Printf("  unified within ~10m:       %d", st.Unified)
	log.Printf("  canonical locations:       created %d, adopted %d", st.CreatedCanonical, st.AdoptedCanonical)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
