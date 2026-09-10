package middleware

import (
	"context"
	"net/http"

	"github.com/devstroop/wam/internal/store"
)

// Identity is the authenticated request principal (populated by the UMS
// session middleware; absent on public/legacy paths).
type Identity struct {
	UserID string
	Email  string
	Name   string
	OrgID  string
	Role   string // admin | user
	// Grants maps wa_account_id → grant role for non-admins (admins skip it).
	Grants map[string]string
	// KeyID is set for API-key auth (sessions leave it empty).
	KeyID string
	// Scopes bound the key (narrowing on top of the role perms).
	Scopes []string
}

// KeyAuth reports whether the principal came from a Bearer API key.
func (id Identity) KeyAuth() bool { return id.KeyID != "" }

// Allows checks perm against key scopes ("*" = all).
func (id Identity) Allows(perm string) bool {
	for _, s := range id.Scopes {
		if s == "*" || s == perm {
			return true
		}
	}
	return false
}

// umsCtxKey namespaces UMS context values apart from request_id.go's ctxKey.
type umsCtxKey string

const (
	identityKey umsCtxKey = "ums-identity"
	scopedDBKey umsCtxKey = "ums-scoped-db"
)

// WithIdentity stashes the principal.
func WithIdentity(ctx context.Context, id Identity) context.Context {
	return context.WithValue(ctx, identityKey, id)
}

// IdentityFrom returns the principal (ok=false on public/legacy paths).
func IdentityFrom(r *http.Request) (Identity, bool) {
	id, ok := r.Context().Value(identityKey).(Identity)
	return id, ok
}

// WithScopedDB stashes the request's org-scoped store handle.
func WithScopedDB(ctx context.Context, db *store.DB) context.Context {
	return context.WithValue(ctx, scopedDBKey, db)
}

// ScopedDB returns the org-scoped handle, or fallback when the request
// carries none (public pages, legacy SQLite path).
func ScopedDB(r *http.Request, fallback *store.DB) *store.DB {
	if db, ok := r.Context().Value(scopedDBKey).(*store.DB); ok && db != nil {
		return db
	}
	return fallback
}
