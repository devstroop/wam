// Package handlers provides shared HTTP helpers.
// Errors use RFC 9457 problem+json (see WriteProblem).
package handlers

import (
	"encoding/json"
	"net/http"

	"github.com/devstroop/wam/internal/middleware"
)

// Problem is an RFC 9457 problem+json payload.
type Problem struct {
	Type     string `json:"type"`
	Title    string `json:"title"`
	Status   int    `json:"status"`
	Detail   string `json:"detail,omitempty"`
	Instance string `json:"instance,omitempty"`
}

// WriteJSON writes a JSON response with status code.
func WriteJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// WriteProblem writes an RFC 9457 problem+json error.
func WriteProblem(w http.ResponseWriter, r *http.Request, status int, title, detail string) {
	w.Header().Set("Content-Type", "application/problem+json")
	w.Header().Set("X-Request-ID", middleware.RequestIDFrom(r.Context()))
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(Problem{
		Type:     "about:blank",
		Title:    title,
		Status:   status,
		Detail:   detail,
		Instance: r.URL.Path,
	})
}
