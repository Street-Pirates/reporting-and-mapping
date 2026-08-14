package api

import (
	"log"
	"net/http"
	"time"
)

func RequestLoggingMiddleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		next.ServeHTTP(w, r)
		log.Printf("%s %q took %dms\n", r.Method, r.RequestURI, time.Since(start).Milliseconds())
	})
}
