package middleware

import (
	"net"
	"net/http"
	"strings"
	"sync"
	"time"
)

// bucket is a token bucket (tokens refill continuously).
type bucket struct {
	tokens float64
	last   time.Time
}

// Limiter is an in-memory token-bucket registry (per-process; sufficient for
// abuse gating — distributed limiting arrives with horizontal scale).
type Limiter struct {
	mu      sync.Mutex
	buckets map[string]*bucket
}

// NewLimiter builds an empty registry.
func NewLimiter() *Limiter { return &Limiter{buckets: map[string]*bucket{}} }

// Allow consumes one token (perMinute refill, burst capacity).
func (l *Limiter) Allow(key string, perMinute, burst int) bool {
	if perMinute <= 0 || burst <= 0 {
		return true // misconfigured = open (fail-open for availability)
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	now := time.Now()
	b, ok := l.buckets[key]
	if !ok {
		b = &bucket{tokens: float64(burst), last: now}
		l.buckets[key] = b
	}
	b.tokens += now.Sub(b.last).Minutes() * float64(perMinute)
	if b.tokens > float64(burst) {
		b.tokens = float64(burst)
	}
	b.last = now
	if b.tokens < 1 {
		return false
	}
	b.tokens--
	return true
}

// ClientIP returns the caller IP (first X-Forwarded-For hop, else remote).
func ClientIP(r *http.Request) string {
	if fwd := r.Header.Get("X-Forwarded-For"); fwd != "" {
		if ip, _, ok := strings.Cut(fwd, ","); ok || ip != "" {
			return strings.TrimSpace(ip)
		}
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}
	return host
}

// OrgKey scopes limits to the identity org (IP fallback for public paths).
func OrgKey(r *http.Request) string {
	if id, ok := IdentityFrom(r); ok && id.OrgID != "" {
		return "org:" + id.OrgID
	}
	return "ip:" + ClientIP(r)
}

// RateLimit rejects over-budget requests with 429 problem+json.
func RateLimit(l *Limiter, scope string, keyFn func(*http.Request) string, perMinute, burst int) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if !l.Allow(scope+"\x00"+keyFn(r), perMinute, burst) {
				w.Header().Set("Content-Type", "application/problem+json")
				w.Header().Set("Retry-After", "60")
				w.WriteHeader(http.StatusTooManyRequests)
				_, _ = w.Write([]byte(`{"title":"Too Many Requests","status":429,"detail":"rate limit exceeded, retry shortly"}`))
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}
