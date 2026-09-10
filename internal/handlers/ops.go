package handlers

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
)

// startedAt anchors uptime (process start, approx).
var startedAt = time.Now()

// Ops serves API keys, outbound webhooks, audit trail and metrics.
type Ops struct {
	Store *store.DB
	Views *views.Views
	WA    *wa.Manager
}

// --- Audit ---

// AuditList returns trail rows (admin: members:manage).
func (h *Ops) AuditList(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermMembersManage)
	if !ok {
		return
	}
	q := r.URL.Query()
	limit, _ := strAtoi(q.Get("limit"), 20)
	entries, next, err := db.ListAudit(id.OrgID, limit, q.Get("cursor"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": entries, "nextCursor": next})
}

func strAtoi(s string, def int) (int, error) {
	var n int
	if _, err := fmt.Sscanf(s, "%d", &n); err != nil || n < 1 {
		return def, nil
	}
	return n, nil
}

// --- API keys (admin: keys:manage) ---

// CreateKey mints a credential (raw secret shown once).
func (h *Ops) CreateKey(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	var body struct {
		Name      string   `json:"name"`
		Scopes    []string `json:"scopes"`
		ExpiresAt string   `json:"expires_at"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Name = r.FormValue("name")
		body.Scopes = r.Form["scopes"]
		body.ExpiresAt = r.FormValue("expires_at")
	}
	if len(body.Scopes) == 0 {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "scopes required")
		return
	}
	// Keys narrow: every scope must be within the creator's own perms.
	for _, s := range body.Scopes {
		if s != "*" && !store.Can(id.Role, s) {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "scope beyond your permissions: "+s)
			return
		}
		if s == "*" && id.Role != store.RoleAdmin {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "wildcard scope needs admin")
			return
		}
	}
	var exp time.Time
	if strings.TrimSpace(body.ExpiresAt) != "" {
		var err error
		exp, err = time.Parse(time.RFC3339, body.ExpiresAt)
		if err != nil || time.Now().After(exp) {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid expires_at (RFC3339 future)")
			return
		}
	}
	key, raw, err := db.CreateAPIKey(id.OrgID, id.UserID, body.Name, body.Scopes, exp)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	Audit(db, id, "keys.create", "api_key", key.ID)
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"API key created — copy it now"}`)
		h.Views.RenderPartial(w, "key-created", map[string]any{"Token": raw, "Prefix": key.Prefix})
		return
	}
	WriteJSON(w, http.StatusCreated, map[string]any{"key": key, "token": raw})
}

// ListKeys returns org credentials (no secrets).
func (h *Ops) ListKeys(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	keys, err := db.ListAPIKeys(id.OrgID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": keys})
}

// RevokeKey kills a credential (row kept for audit).
func (h *Ops) RevokeKey(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	if err := db.RevokeAPIKey(r.PathValue("id"), id.OrgID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	Audit(db, id, "keys.revoke", "api_key", r.PathValue("id"))
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"API key revoked"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Outbound webhooks (admin: keys:manage — same trust level) ---

// CreateEndpoint registers a target.
func (h *Ops) CreateEndpoint(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	var body struct {
		URL    string   `json:"url"`
		Secret string   `json:"secret"`
		Events []string `json:"events"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.URL == "" {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "url is required")
			return
		}
	} else {
		_ = r.ParseForm()
		body.URL = r.FormValue("url")
		body.Secret = r.FormValue("secret")
		body.Events = r.Form["events"]
		if body.URL == "" {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "url is required")
			return
		}
	}
	e, err := db.CreateEndpoint(id.OrgID, body.URL, body.Secret, body.Events, id.UserID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	Audit(db, id, "webhooks.create", "webhook_endpoint", e.ID)
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Webhook added"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusCreated)
		return
	}
	WriteJSON(w, http.StatusCreated, e)
}

// ListEndpoints returns targets (no secrets).
func (h *Ops) ListEndpoints(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	endpoints, err := db.ListEndpoints(id.OrgID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": endpoints, "events": store.WebhookEvents})
}

// DeleteEndpoint removes a target.
func (h *Ops) DeleteEndpoint(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermKeysManage)
	if !ok {
		return
	}
	if err := db.DeleteEndpoint(r.PathValue("id"), id.OrgID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	Audit(db, id, "webhooks.delete", "webhook_endpoint", r.PathValue("id"))
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Webhook deleted"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Metrics (any member; scrape with an API key) ---

// Metrics exposes Prometheus-text headline gauges.
func (h *Ops) Metrics(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	contacts, groups, campaigns, queued := 0, 0, 0, 0
	if db != nil {
		contacts, groups, campaigns, queued = db.Stats()
	}
	accountsTotal, accountsLive := 0, 0
	if db != nil && db.IsPostgres() {
		if accounts, err := db.AccountsByOrg(id.OrgID); err == nil {
			for _, a := range visibleAccounts(id, accounts) {
				accountsTotal++
				if h.WA != nil {
					if st := h.WA.Status(a.ID); st.Connected && st.LoggedIn {
						accountsLive++
					}
				}
			}
		}
	}
	uptime := int(time.Since(startedAt).Seconds())
	w.Header().Set("Content-Type", "text/plain; version=0.0.4")
	fmt.Fprintf(w, `# HELP wam_contacts Total contacts (org scope).
# TYPE wam_contacts gauge
wam_contacts %d
# HELP wam_groups Total groups (org scope).
# TYPE wam_groups gauge
wam_groups %d
# HELP wam_campaigns Total campaigns (org scope).
# TYPE wam_campaigns gauge
wam_campaigns %d
# HELP wam_queued Queued recipients (org scope).
# TYPE wam_queued gauge
wam_queued %d
# HELP wam_accounts_total WhatsApp numbers (org scope).
# TYPE wam_accounts_total gauge
wam_accounts_total %d
# HELP wam_accounts_connected Connected numbers (org scope).
# TYPE wam_accounts_connected gauge
wam_accounts_connected %d
# HELP wam_uptime_seconds Process uptime.
# TYPE wam_uptime_seconds counter
wam_uptime_seconds %d
`, contacts, groups, campaigns, queued, accountsTotal, accountsLive, uptime)
}
