package middleware

import (
	"log/slog"
	"net/http"
	"time"
)

type statusRecorder struct {
	http.ResponseWriter
	status int
	bytes  int
}

func (r *statusRecorder) WriteHeader(code int) {
	r.status = code
	r.ResponseWriter.WriteHeader(code)
}

func (r *statusRecorder) Write(b []byte) (int, error) {
	if r.status == 0 {
		r.status = http.StatusOK
	}
	n, err := r.ResponseWriter.Write(b)
	r.bytes += n
	return n, err
}

// Logger emits one structured log line per request, with level
// routed by concern: API at INFO, frontend/noisy at DEBUG, errors
// at WARN/ERROR. With the default WAM_LOG_LEVEL=info, this means
// terminal shows API traffic cleanly while page loads, htmx polls
// (/partials/* every 15s), static assets and health probes stay
// hidden unless you run with WAM_LOG_LEVEL=debug.
//
// Rationale: WhatsApp Marketing is API-first; frontend hits are
// an order of magnitude noisier than API calls and drown the signal.
// See docs: keep API=INFO, web=DEBUG, 4xx=WARN, 5xx=ERROR, slow(>1s)=WARN.
func Logger(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)
			dur := time.Since(start)
			attrs := []any{
				"method", r.Method,
				"path", r.URL.Path,
				"query", r.URL.RawQuery,
				"status", rec.status,
				"bytes", rec.bytes,
				"dur_ms", dur.Milliseconds(),
				"request_id", RequestIDFrom(r.Context()),
				"hx_request", r.Header.Get("HX-Request") != "",
			}
			// Route by status first — errors always surface.
			if rec.status >= 500 {
				log.Error("http request", attrs...)
				return
			}
			if rec.status >= 400 {
				log.Warn("http request", attrs...)
				return
			}
			if dur > 1000*time.Millisecond {
				// Slow even for frontend — worth INFO.
				log.Warn("http request slow", attrs...)
				return
			}
			if isAPI(r.URL.Path) {
				log.Info("http request", attrs...)
				return
			}
			// Everything else is web/noise: pages, partials, static, docs, probes.
			// Emits at DEBUG so it is hidden at the default INFO level.
			// Run with WAM_LOG_LEVEL=debug to see it.
			log.Debug("http request", attrs...)
		})
	}
}

func isAPI(path string) bool {
	// API + docs that are effectively API surface for debugging.
	// Keep narrow: only /api/* is INFO. Docs/probes/static are DEBUG.
	return len(path) >= 4 && path[0:4] == "/api"
}

// Recover converts panics into 500 responses.
func Recover(log *slog.Logger) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			defer func() {
				if rec := recover(); rec != nil {
					log.Error("panic recovered", "panic", rec, "path", r.URL.Path)
					http.Error(w, http.StatusText(http.StatusInternalServerError), http.StatusInternalServerError)
				}
			}()
			next.ServeHTTP(w, r)
		})
	}
}

// SecurityHeaders sets a minimal secure baseline. Adjust CSP when adding CDNs.
func SecurityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("X-Content-Type-Options", "nosniff")
		w.Header().Set("X-Frame-Options", "DENY")
		w.Header().Set("Referrer-Policy", "no-referrer")
		next.ServeHTTP(w, r)
	})
}
