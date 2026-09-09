package middleware

import "net/http"

// CORS is a permissive development default. Tighten AllowedOrigins in production
// or replace with an allow-list loaded from config.
func CORS(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Access-Control-Allow-Origin", "*")
		w.Header().Set("Access-Control-Allow-Methods", "GET, POST, PUT, PATCH, DELETE, OPTIONS")
		w.Header().Set("Access-Control-Allow-Headers", "Content-Type, Authorization, X-Request-ID, HX-Request, HX-Trigger, HX-Target")
		w.Header().Set("Vary", "Origin")
		if r.Method == http.MethodOptions {
			w.WriteHeader(http.StatusNoContent)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// IsHTMX reports whether the request came from htmx (HX-Request: true).
// Handlers use this to return partials instead of full pages.
func IsHTMX(r *http.Request) bool {
	return r.Header.Get("HX-Request") == "true"
}
