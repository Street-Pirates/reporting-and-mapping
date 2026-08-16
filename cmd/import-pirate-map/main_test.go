package main

import (
	"encoding/xml"
	"testing"
	"time"
)

func TestClassify(t *testing.T) {
	cases := map[string]string{
		// Real folder names from the Pirate Map.
		"Reported Signs":                              classExists,
		"Major Campaign Signs Reported":               classExists,
		"Street Pirate Plunders":                      classGone,
		"Major Plunders - JS":                         classGone,
		"Major Plunders - JICR Pre-2026":              classGone,
		"Major Plunders - JICR 2026+":                 classGone,
		"Removed but not by ASP":                      classGone,
		"Major Campaign signs removed but not by ASP": classGone,
		"Sightings of Questionable Legality":          classDiscard,
		"Billboards":                                  classDiscard,
		// Keyword variants the rule is meant to catch.
		"Plundered signs": classGone,
		"REMOVED":         classGone,
		"whatever else":   classUnknown,
	}
	for name, want := range cases {
		if got := classify(name); got != want {
			t.Errorf("classify(%q) = %q, want %q", name, got, want)
		}
	}
}

func TestBuildRecordsSetsMessageFromFolderName(t *testing.T) {
	doc := kmlDoc{}
	doc.Document.Folders = []kmlFolder{
		{Name: "Major Plunders - JS", Placemarks: []kmlPlace{{Name: "sign 1", Coordinates: "-117.1,34.1,0"}}},
		{Name: "Major Plunders - JICR Pre-2026", Placemarks: []kmlPlace{{Name: "sign 2", Coordinates: "-117.2,34.2,0"}}},
	}

	recs, _ := buildRecords(doc, "test", time.Now().UTC())
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if got := recs[0].Message; got != "js" {
		t.Errorf("first record message = %q, want %q", got, "js")
	}
	if got := recs[1].Message; got != "jicr" {
		t.Errorf("second record message = %q, want %q", got, "jicr")
	}
}

func TestBuildRecordsSetsMessageFromDescriptionFallback(t *testing.T) {
	doc := kmlDoc{}
	doc.Document.Folders = []kmlFolder{{
		Name: "Reported Signs",
		Placemarks: []kmlPlace{
			{Name: "sign 1", Coordinates: "-117.1,34.1,0", Description: "Jesus is coming"},
			{Name: "sign 2", Coordinates: "-117.2,34.2,0", Description: "Jesus saves"},
			{Name: "sign 3", Coordinates: "-117.3,34.3,0", Description: "Believe in Jesus"},
		},
	}}

	recs, _ := buildRecords(doc, "test", time.Now().UTC())
	if len(recs) != 3 {
		t.Fatalf("records = %d, want 3", len(recs))
	}
	if got := recs[0].Message; got != "jicr" {
		t.Errorf("first record message = %q, want %q", got, "jicr")
	}
	if got := recs[1].Message; got != "js" {
		t.Errorf("second record message = %q, want %q", got, "js")
	}
	if got := recs[2].Message; got != "bij" {
		t.Errorf("third record message = %q, want %q", got, "bij")
	}
}

func TestBuildRecordsSetsMediumFromDescription(t *testing.T) {
	doc := kmlDoc{}
	doc.Document.Folders = []kmlFolder{{
		Name: "Reported Signs",
		Placemarks: []kmlPlace{
			{Name: "placard", Coordinates: "-117.1,34.1,0", Description: "Jesus is coming"},
			{Name: "sticker", Coordinates: "-117.2,34.2,0", Description: "A sticker for Jesus"},
		},
	}}

	recs, _ := buildRecords(doc, "test", time.Now().UTC())
	if len(recs) != 2 {
		t.Fatalf("records = %d, want 2", len(recs))
	}
	if got := recs[0].Medium; got != "placard" {
		t.Errorf("first record medium = %q, want %q", got, "placard")
	}
	if got := recs[1].Medium; got != "sticker" {
		t.Errorf("second record medium = %q, want %q", got, "sticker")
	}
}

func TestNaturalIDStableAndDistinct(t *testing.T) {
	const src = "pirate-map"
	base := naturalID(src, "CA - Jesus (Costa Mesa)", 34.401004, -117.57906)

	// Deterministic: identical inputs -> identical id.
	if again := naturalID(src, "CA - Jesus (Costa Mesa)", 34.401004, -117.57906); again != base {
		t.Errorf("naturalID not deterministic: %q vs %q", base, again)
	}
	// Namespaced by source.
	if base[:len(src)+1] != src+":" {
		t.Errorf("naturalID = %q, want %q prefix", base, src+":")
	}
	// Rounding to 6 dp: excess precision does not change the id (stable across
	// re-exports), ...
	if stable := naturalID(src, "CA - Jesus (Costa Mesa)", 34.4010044999, -117.5790600001); stable != base {
		t.Errorf("naturalID not stable under trailing precision: %q vs %q", base, stable)
	}
	// ... but a different name or a meaningfully different location does.
	if other := naturalID(src, "CA - Different (Costa Mesa)", 34.401004, -117.57906); other == base {
		t.Error("naturalID collision across different names")
	}
	if other := naturalID(src, "CA - Jesus (Costa Mesa)", 34.402004, -117.57906); other == base {
		t.Error("naturalID collision across different coordinates")
	}
}

func TestParseLonLat(t *testing.T) {
	// KML order is lon,lat[,alt]; My Maps wraps the body in whitespace.
	lat, lon, err := parseLonLat("\n   -117.57906,34.401004,0\n  ")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if lat != 34.401004 || lon != -117.57906 {
		t.Errorf("got lat=%v lon=%v, want lat=34.401004 lon=-117.57906", lat, lon)
	}
	// Multi-tuple bodies (defensive): take the first tuple.
	if lat, lon, err = parseLonLat("1.5,2.5,0 3.5,4.5,0"); err != nil || lat != 2.5 || lon != 1.5 {
		t.Errorf("multi-tuple: lat=%v lon=%v err=%v", lat, lon, err)
	}
	for _, bad := range []string{"", "   ", "onlyone"} {
		if _, _, err := parseLonLat(bad); err == nil {
			t.Errorf("parseLonLat(%q) = nil error, want error", bad)
		}
	}
}
func TestObservationDateOverrideFromDescription(t *testing.T) {
	defaultWhen := time.Date(2026, 6, 20, 22, 22, 0, 0, time.UTC)
	got, ok := observationDateFromDescription(defaultWhen, "Jesus is coming<br>07/25/26<br>More text")
	if !ok {
		t.Fatal("observationDateFromDescription returned ok=false, want true")
	}
	want := time.Date(2026, 7, 25, 4, 20, 0, 0, time.UTC)
	if !got.Equal(want) {
		t.Fatalf("got %s, want %s", got.Format(time.RFC3339), want.Format(time.RFC3339))
	}

	if _, ok := observationDateFromDescription(defaultWhen, "Jesus is coming<br>06/01/26<br>More text"); ok {
		t.Fatal("expected earlier date to be ignored")
	}
}
// TestParseKMLShape verifies the struct tags decode a My-Maps-shaped document:
// a default kml namespace, folders, and points with whitespace-wrapped coords.
func TestParseKMLShape(t *testing.T) {
	const doc = `<?xml version="1.0"?>
<kml xmlns="http://www.opengis.net/kml/2.2"><Document>
  <name>Test Map</name>
  <Folder>
    <name>Reported Signs</name>
    <Placemark><name>Sign 1</name><Point><coordinates>-117.1,34.1,0</coordinates></Point></Placemark>
  </Folder>
  <Folder>
    <name>Billboards</name>
    <Placemark><name>BB</name><Point><coordinates>-118.2,35.2,0</coordinates></Point></Placemark>
  </Folder>
</Document></kml>`

	var k kmlDoc
	if err := xml.Unmarshal([]byte(doc), &k); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if k.Document.Name != "Test Map" {
		t.Errorf("doc name = %q", k.Document.Name)
	}
	if len(k.Document.Folders) != 2 {
		t.Fatalf("folders = %d, want 2", len(k.Document.Folders))
	}
	recs, sum := buildRecords(k, "test", time.Now().UTC())
	if len(recs) != 1 {
		t.Fatalf("records = %d, want 1 (Billboards discarded)", len(recs))
	}
	if !recs[0].Exists || recs[0].Description != "Sign 1" {
		t.Errorf("record = %+v", recs[0])
	}
	if sum.discarded != 1 {
		t.Errorf("discarded = %d, want 1", sum.discarded)
	}
}
