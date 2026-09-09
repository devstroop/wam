package auth

import (
	"net/http"
	"strings"

	"github.com/devstroop/wam/internal/middleware"
)

// RequireAdmin guards dashboard pages + APIs. Public: landing, login,
// health probes, docs, static, partials used by public pages.
// HTML navigations redirect to /login; API/partials get 401 JSON.
func (s *Session) RequireAdmin(next http.Handler) http.Handler {
	public := []string{
		"/", "/login", "/healthz", "/readyz",
		"/api-docs", "/api-docs/", "/api-docs/openapi.yaml",
		"/openapi.yaml", "/static/",
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !s.Enabled() || s.Valid(r) {
			next.ServeHTTP(w, r)
			return
		}
		for _, p := range public {
			if r.URL.Path == p {
				next.ServeHTTP(w, r)
				return
			}
			// Prefix match for subtrees (e.g. /static/*), but never for root "/".
			if len(p) > 1 && strings.HasSuffix(p, "/") && strings.HasPrefix(r.URL.Path, p) {
				next.ServeHTTP(w, r)
				return
			}
		}
		if middleware.IsHTMX(r) || strings.HasPrefix(r.URL.Path, "/api/") {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"admin login required"}`))
			return
		}
		http.Redirect(w, r, "/login", http.StatusSeeOther)
	})
}
