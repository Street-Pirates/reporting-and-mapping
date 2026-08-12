package api

import (
	"encoding/json"
	"io"
	"net/http"
)

func validLatLon(lat, lon float64) bool {
	return lat >= -90 && lat <= 90 && lon >= -180 && lon <= 180
}

// decodeOptional decodes a JSON body if present, treating an empty body as OK.
func decodeOptional(r *http.Request, dst any) error {
	err := json.NewDecoder(io.LimitReader(r.Body, 1<<20)).Decode(dst)
	if err == io.EOF {
		return nil
	}
	return err
}
