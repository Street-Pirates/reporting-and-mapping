// Package geo provides small geospatial helpers used for the "within 100m"
// location reconciliation radius.
package geo

import "math"

const earthRadiusM = 6371000.0 // mean Earth radius in metres

// DistanceMeters returns the great-circle (haversine) distance in metres
// between two WGS-84 coordinates.
func DistanceMeters(lat1, lon1, lat2, lon2 float64) float64 {
	p1 := lat1 * math.Pi / 180
	p2 := lat2 * math.Pi / 180
	dPhi := (lat2 - lat1) * math.Pi / 180
	dLambda := (lon2 - lon1) * math.Pi / 180

	a := math.Sin(dPhi/2)*math.Sin(dPhi/2) +
		math.Cos(p1)*math.Cos(p2)*math.Sin(dLambda/2)*math.Sin(dLambda/2)
	c := 2 * math.Atan2(math.Sqrt(a), math.Sqrt(1-a))
	return earthRadiusM * c
}

// BBox is a latitude/longitude bounding box used as a cheap SQL pre-filter
// before the exact haversine test.
type BBox struct {
	MinLat, MaxLat, MinLon, MaxLon float64
}

// BoundingBox returns a box that fully contains the circle of the given radius
// (metres) around the point. It is intentionally slightly generous; callers
// must still apply DistanceMeters for an exact test.
func BoundingBox(lat, lon, radiusM float64) BBox {
	dLat := radiusM / 111320.0 // metres per degree latitude (approx, constant)
	// metres per degree longitude shrinks with latitude.
	cosLat := math.Cos(lat * math.Pi / 180)
	if cosLat < 1e-6 {
		cosLat = 1e-6 // guard against the poles
	}
	dLon := radiusM / (111320.0 * cosLat)
	return BBox{
		MinLat: lat - dLat, MaxLat: lat + dLat,
		MinLon: lon - dLon, MaxLon: lon + dLon,
	}
}
