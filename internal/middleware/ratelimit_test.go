package middleware

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

// TestLimiterAllow covers burst + refuse + key isolation.
func TestLimiterAllow(t *testing.T) {
	l := NewLimiter()
	for i := 0; i < 3; i++ {
		if !l.Allow("k", 60, 3) {
			t.Fatalf("burst %d denied", i)
		}
	}
	if l.Allow("k", 60, 3) {
		t.Fatal("over-burst allowed")
	}
	if !l.Allow("other", 60, 3) {
		t.Fatal("separate key denied")
	}
	if !l.Allow("open", 0, 0) {
		t.Fatal("misconfigured limiter should fail open")
	}
}

// TestRateLimitMiddleware checks the 429 shape and pass-through.
func TestRateLimitMiddleware(t *testing.T) {
	l := NewLimiter()
	ok := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) { w.WriteHeader(http.StatusOK) })
	h := RateLimit(l, "t", func(*http.Request) string { return "k" }, 60, 1)(ok)

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("first = %d", rec.Code)
	}
	rec = httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest("GET", "/", nil))
	if rec.Code != http.StatusTooManyRequests {
		t.Fatalf("second = %d, want 429", rec.Code)
	}
	if rec.Header().Get("Retry-After") == "" {
		t.Fatal("missing Retry-After")
	}
}

// TestClientIP covers proxy header handling.
func TestClientIP(t *testing.T) {
	r := httptest.NewRequest("GET", "/", nil)
	r.Header.Set("X-Forwarded-For", "1.2.3.4, 5.6.7.8")
	if got := ClientIP(r); got != "1.2.3.4" {
		t.Fatalf("ip = %q", got)
	}
	r2 := httptest.NewRequest("GET", "/", nil)
	r2.RemoteAddr = "9.9.9.9:1234"
	if got := ClientIP(r2); got != "9.9.9.9" {
		t.Fatalf("ip = %q", got)
	}
}
