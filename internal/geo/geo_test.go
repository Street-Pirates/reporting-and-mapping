package geo

import (
	"math"
	"testing"
)

func TestDistanceMeters(t *testing.T) {
	// ~30m apart (used by the 100m-radius reconciliation tests).
	d := DistanceMeters(37.7750, -122.4195, 37.77527, -122.4195)
	if math.Abs(d-30) > 3 {
		t.Fatalf("expected ~30m, got %.1fm", d)
	}
	// Identical points -> 0.
	if d0 := DistanceMeters(1, 2, 1, 2); d0 != 0 {
		t.Fatalf("expected 0m for identical points, got %v", d0)
	}
}

func TestBoundingBoxContainsRadius(t *testing.T) {
	lat, lon := 37.7750, -122.4195
	bb := BoundingBox(lat, lon, 100)
	// A point due north at exactly 100m must fall inside the box.
	northLat := lat + 100.0/111320.0
	if northLat > bb.MaxLat {
		t.Fatalf("100m-north point %.6f exceeds box max %.6f", northLat, bb.MaxLat)
	}
}
