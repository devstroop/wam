// Package handlers provides shared HTTP helpers.
// Errors use RFC 9457 problem+json (see WriteProblem).
package handlers

import (
	"encoding/json"
	"net/http"
	"strings"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
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

// Authorize resolves the request's org-scoped store handle and enforces perm.
// Reads pass "" (any org member). Legacy SQLite requests carry no identity
// and pass through (single-admin authorized at the middleware). Returns
// ok=false after writing the 403/redirect — callers must return.
func Authorize(fallback *store.DB, w http.ResponseWriter, r *http.Request, perm string) (middleware.Identity, *store.DB, bool) {
	db := middleware.ScopedDB(r, fallback)
	id, ok := middleware.IdentityFrom(r)
	if !ok {
		return middleware.Identity{}, db, true
	}
	if perm != "" && !store.Can(id.Role, perm) {
		if middleware.IsHTMX(r) || strings.HasPrefix(r.URL.Path, "/api/") {
			WriteProblem(w, r, http.StatusForbidden, "Forbidden", "missing permission "+perm)
		} else {
			http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
		}
		return middleware.Identity{}, nil, false
	}
	// API keys narrow further to their scopes (sessions skip this).
	if id.KeyAuth() && perm != "" && !id.Allows(perm) {
		WriteProblem(w, r, http.StatusForbidden, "Forbidden", "key scope missing "+perm)
		return middleware.Identity{}, nil, false
	}
	return id, db, true
}

// Audit appends a trail row (never fails the request; no-op on legacy).
func Audit(db *store.DB, id middleware.Identity, action, entity, entityID string) {
	if db == nil || !db.IsPostgres() {
		return
	}
	_ = db.Audit(id.OrgID, id.UserID, id.Email, action, entity, entityID, "")
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
