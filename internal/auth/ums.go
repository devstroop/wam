package auth

import (
	"database/sql"
	"errors"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
)

// sessionTTL matches the HMAC cookie lifetime.
const sessionTTL = 30 * 24 * time.Hour

// umsPublic are paths that bypass session checks on the Postgres path.
var umsPublic = []string{
	"/", "/login", "/signup", "/verify", "/forgot", "/reset", "/invite/accept",
	"/healthz", "/readyz",
	"/api-docs", "/api-docs/", "/api-docs/openapi.yaml",
	"/openapi.yaml", "/static/",
	"/api/v1/auth/login", "/api/v1/auth/signup",
	"/api/v1/auth/verify", "/api/v1/auth/forgot", "/api/v1/auth/reset",
}

func isUMSPublic(path string) bool {
	for _, p := range umsPublic {
		if path == p {
			return true
		}
		if len(p) > 1 && strings.HasSuffix(p, "/") && strings.HasPrefix(path, p) {
			return true
		}
	}
	return false
}

// RequireUMS replaces RequireAdmin on the Postgres path: HMAC cookie +
// server-side session row, org membership, per-request org transaction.
// Unauthenticated HTML navigations redirect to /login; API/htmx get 401.
func (s *Session) RequireUMS(db *store.DB, log *slog.Logger) func(http.Handler) http.Handler {
	if log == nil {
		log = slog.Default()
	}
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if isUMSPublic(r.URL.Path) {
				next.ServeHTTP(w, r)
				return
			}
			deny := func() {
				s.Clear(w)
				if middleware.IsHTMX(r) || strings.HasPrefix(r.URL.Path, "/api/") {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusUnauthorized)
					_, _ = w.Write([]byte(`{"title":"Unauthorized","status":401,"detail":"login required"}`))
					return
				}
				http.Redirect(w, r, "/login", http.StatusSeeOther)
			}
			raw, ok := s.CookieValue(r)
			if !ok || !s.Valid(r) {
				deny()
				return
			}
			srow, err := db.GetSessionByTokenHash(HashToken(raw))
			if err != nil {
				deny()
				return
			}
			user, err := db.GetUser(srow.UserID)
			if err != nil {
				deny()
				return
			}
			memberships, err := db.MembershipsByUser(user.ID)
			if err != nil || len(memberships) == 0 {
				deny()
				return
			}
			// Session org may have been revoked; fall back to the first
			// remaining membership so one removal doesn't lock the user out.
			orgID, role := srow.OrgID, ""
			for _, m := range memberships {
				if m.OrgID == orgID {
					role = m.Role
				}
			}
			if role == "" {
				orgID, role = memberships[0].OrgID, memberships[0].Role
				_ = db.SetSessionOrg(HashToken(raw), orgID)
			}
			tx, err := db.BeginOrgTx(r.Context(), orgID)
			if err != nil {
				log.Warn("ums: org tx failed", "err", err)
				deny()
				return
			}
			committed := false
			defer func() {
				if !committed {
					_ = tx.Rollback()
				}
			}()
			// Identity reads run inside the org transaction: grants carry
			// fail-closed RLS (009) and are invisible to unscoped handles.
			odb := tx.DB(db.Driver)
			grants := map[string]string{}
			if role != store.RoleAdmin {
				if gs, err := odb.GrantsForUser(user.ID, orgID); err == nil {
					for _, g := range gs {
						grants[g.AccountID] = g.Role
					}
				}
			}
			ctx := middleware.WithScopedDB(r.Context(), odb)
			ctx = middleware.WithIdentity(ctx, middleware.Identity{
				UserID: user.ID, Email: user.Email, Name: user.Name,
				OrgID: orgID, Role: role, Grants: grants,
			})
			next.ServeHTTP(w, r.WithContext(ctx))
			committed = true
			if err := tx.Commit(); err != nil {
				log.Warn("ums: org tx commit failed", "err", err)
			}
		})
	}
}

// ErrNoMember signals a missing membership (maps to 404 to avoid probing).
var ErrNoMember = errors.New("not a member")

// MembershipRole loads the caller's role for an org (usually equals the
// identity role; re-read when acting across the session org).
func MembershipRole(db *store.DB, userID, orgID string) (string, error) {
	m, err := db.GetMembership(userID, orgID)
	if err != nil {
		if err == sql.ErrNoRows {
			return "", ErrNoMember
		}
		return "", err
	}
	return m.Role, nil
}
