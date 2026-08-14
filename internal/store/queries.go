package store

import (
	"strings"

	"sightingmap/internal/geo"
)

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
