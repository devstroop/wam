package handlers

import (
	"context"
	"encoding/json"
	"fmt"
	"html/template"
	"net/http"
	"strings"
	"time"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
)

// Connection serves the WhatsApp lifecycle.
// JSON for API clients; HTML fragments when HX-Request is present.
// Legacy single-account routes resolve the default account; per-account
// routes live under /api/v1/accounts (see accounts.go).
type Connection struct {
	WA    *wa.Manager
	Views *views.Views
	Store *store.DB
}

// accountCtx resolves the target account: ?account= when given (view/use
// checked), else the org's default (sole account). Legacy returns the
// implicit account. ok=false after writing the error response.
func (h *Connection) accountCtx(w http.ResponseWriter, r *http.Request, perm string, needUse bool) (id middleware.Identity, db *store.DB, accountID, deviceJID string, ok bool) {
	id, db, ok = Authorize(h.Store, w, r, perm)
	if !ok {
		return middleware.Identity{}, nil, "", "", false
	}
	if !db.IsPostgres() {
		return id, db, store.LegacyAccountID, "", true
	}
	want := r.URL.Query().Get("account")
	if want == "" {
		want = r.PathValue("account_id")
	}
	var aid string
	var err error
	if needUse {
		aid, err = ResolveSenderAccount(db, id, want)
	} else {
		aid, err = ResolveViewerAccount(db, id, want)
	}
	if err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error())
		return middleware.Identity{}, nil, "", "", false
	}
	a, err := db.GetAccount(aid)
	if err != nil {
		writeStoreError(w, r, err)
		return middleware.Identity{}, nil, "", "", false
	}
	return id, db, a.ID, a.DeviceJID, true
}

// pairingCtx resolves the pairing target for QR/Pair: explicit ?account=
// (use-checked) or the org default — auto-creating a pending row when the
// org has none, so first-time pairing never dead-ends. Legacy returns the
// implicit account. ok=false after writing the error response.
func (h *Connection) pairingCtx(w http.ResponseWriter, r *http.Request) (id middleware.Identity, db *store.DB, accountID, deviceJID string, ok bool) {
	id, db, ok = Authorize(h.Store, w, r, store.PermAccountsPair)
	if !ok {
		return middleware.Identity{}, nil, "", "", false
	}
	if !db.IsPostgres() {
		return id, db, store.LegacyAccountID, "", true
	}
	want := r.URL.Query().Get("account")
	if want == "" {
		want = r.PathValue("account_id")
	}
	a, err := db.EnsurePairingAccount(id.OrgID, want, id.UserID)
	if err != nil {
		if want != "" {
			writeStoreError(w, r, err)
		} else {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", err.Error())
		}
		return middleware.Identity{}, nil, "", "", false
	}
	if want != "" && !store.CanAccount(id.Role, id.Grants, a.ID, true) {
		WriteProblem(w, r, http.StatusForbidden, "Forbidden", "no access to that WhatsApp account")
		return middleware.Identity{}, nil, "", "", false
	}
	return id, db, a.ID, a.DeviceJID, true
}

// Status returns live connection state.
func (h *Connection) Status(w http.ResponseWriter, r *http.Request) {
	_, _, accountID, _, ok := h.accountCtx(w, r, store.PermAccountsView, false)
	if !ok {
		return
	}
	WriteJSON(w, http.StatusOK, h.WA.Status(accountID))
}

// QR returns a pairing QR: JSON {qr, expiresIn} for API, <img> for htmx.
// Times out after ~45s waiting for the first code event.
func (h *Connection) QR(w http.ResponseWriter, r *http.Request) {
	_, _, accountID, deviceJID, ok := h.pairingCtx(w, r)
	if !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	dataURL, err := h.WA.QRCodePNG(ctx, accountID, deviceJID)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<img src="` + template.HTMLEscapeString(dataURL) + `" alt="WhatsApp QR" style="max-width:256px;border-radius:8px" /><p class="muted">Scan within 2 minutes, then watch status above.</p>`))
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"qr": dataURL, "expiresIn": 120})
}

// Pair starts phone-number linking: JSON {code} or HTML fragment.
func (h *Connection) Pair(w http.ResponseWriter, r *http.Request) {
	_, _, accountID, deviceJID, ok := h.pairingCtx(w, r)
	if !ok {
		return
	}
	var phone string
	if r.Header.Get("Content-Type") == "application/json" {
		var body struct {
			Phone string `json:"phone"`
		}
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
		phone = body.Phone
	} else {
		_ = r.ParseForm()
		phone = r.FormValue("phone")
	}
	if phone == "" {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "phone is required (E.164)")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	code, err := h.WA.PairPhone(ctx, accountID, deviceJID, phone)
	if err != nil {
		h.fail(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		_, _ = w.Write([]byte(`<p>Enter this code on your phone:<br /><strong style="font-size:1.5rem;letter-spacing:.25rem">` + template.HTMLEscapeString(code) + `</strong></p><p class="muted">WhatsApp → Linked devices → Link with phone number.</p>`))
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"code": code})
}

// Logout disconnects and wipes the session.
func (h *Connection) Logout(w http.ResponseWriter, r *http.Request) {
	_, db, accountID, deviceJID, ok := h.accountCtx(w, r, store.PermAccountsPair, true)
	if !ok {
		return
	}
	if err := h.WA.RemoveDevice(accountID, deviceJID); err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Logout failed", err.Error())
		return
	}
	if db.IsPostgres() {
		_ = db.MarkAccountLoggedOut(accountID)
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// Detail renders the rich status card for the Connect page (distinct from the
// compact header badge). Polled live; shows phone + push name + state.
func (h *Connection) Detail(w http.ResponseWriter, r *http.Request) {
	_, _, accountID, _, ok := h.accountCtx(w, r, store.PermAccountsView, false)
	if !ok {
		return
	}
	st := h.WA.Status(accountID)
	if h.Views == nil {
		// Fallback when views unavailable (tests): compact badge.
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		badge := `<span class="badge badge-warning"><span class="badge-dot"></span>Not Connected</span>`
		if st.Connected && st.LoggedIn {
			badge = `<span class="badge badge-success"><span class="badge-dot"></span>` + template.HTMLEscapeString(st.Phone) + `</span>`
		} else if st.Connected {
			badge = `<span class="badge badge-warning"><span class="badge-dot"></span>Linking…</span>`
		}
		_, _ = w.Write([]byte(badge))
		return
	}
	h.Views.RenderPartial(w, "connection-detail", map[string]any{
		"Connected": st.Connected, "LoggedIn": st.LoggedIn,
		"Phone": st.Phone, "PushName": st.PushName,
	})
}

// Sidebar renders one connection block per viewable account (polled every
// 15s). Legacy renders the single implicit account. Each block opens the
// dialog for its account.
func (h *Connection) Sidebar(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermAccountsView)
	if !ok {
		return
	}
	type row struct {
		ID, Label, Phone, Sub, Class string
		Live                         bool
	}
	rows := []row{}
	addRow := func(id, label string, st wa.Status) {
		display := st.Phone
		switch {
		case st.Connected && st.LoggedIn:
			sub := "Connected"
			if label != "" {
				sub = label + " · Connected"
			}
			rows = append(rows, row{id, label, display, sub, "account-chip", true})
		case st.Connected:
			rows = append(rows, row{id, label, "Linking…", "Linking…", "link-device-btn is-linking", false})
		default:
			name := "Link a device"
			if label != "" {
				name = label + " · Not linked"
			}
			rows = append(rows, row{id, label, name, "Not linked", "link-device-btn is-off", false})
		}
	}
	if db != nil && db.IsPostgres() {
		if accounts, err := db.AccountsByOrg(id.OrgID); err == nil {
			for _, a := range visibleAccounts(id, accounts) {
				label := a.Label
				if label == "" {
					label = a.Phone
				}
				addRow(a.ID, label, h.WA.Status(a.ID))
			}
		}
		if len(rows) == 0 {
			rows = append(rows, row{"", "", "Link a device", "Pair an account in Connect", "link-device-btn is-off", false})
		}
	} else {
		addRow(store.LegacyAccountID, "", h.WA.Status(store.LegacyAccountID))
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	const waSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 21l1.65-3.8a9 9 0 1 1 3.4 2.9L3 21"/><path d="M9.5 9a.5.5 0 0 0 1 0V9a.5.5 0 0 0-1 0v1a5 5 0 0 0 5 5h1a.5.5 0 0 0 0-1h-1a.5.5 0 0 0 0 1"/></svg>`
	var out strings.Builder
	for _, rw := range rows {
		dot := `<span class="badge-dot"></span>`
		if rw.Live {
			dot = `<span class="badge-dot is-live"></span>`
		}
		fmt.Fprintf(&out, `<button type="button" class="%s" onclick="openConnectDialog(%s)" title="%s">`+
			`<span class="account-chip-icon">%s</span>`+
			`<span class="sidebar-text account-meta"><span class="account-phone">%s</span><span class="account-sub">%s</span></span>%s</button>`,
			rw.Class, jsStr(rw.ID), template.HTMLEscapeString(rw.Phone),
			waSVG, template.HTMLEscapeString(rw.Phone), template.HTMLEscapeString(rw.Sub), dot)
	}
	_, _ = w.Write([]byte(out.String()))
}

// jsStr quotes a string for inline JS use.
func jsStr(s string) string {
	if s == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(s, "'", "\\'") + "'"
}

// Dialog renders the connect dialog body: rich status + disconnect when
// linked, otherwise Scan QR / Pair tabs. Never 400s on ambiguity: zero
// accounts renders the unpaired notice, multiple accounts without an
// explicit ?account= renders a picker. Only unknown/inaccessible explicit
// ids error.
func (h *Connection) Dialog(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermAccountsView)
	if !ok {
		return
	}
	if h.Views == nil {
		http.Error(w, "views unavailable", http.StatusInternalServerError)
		return
	}
	renderStatus := func(accountID string) {
		st := h.WA.Status(accountID)
		h.Views.RenderPartial(w, "connect-dialog", map[string]any{
			"Connected": st.Connected, "LoggedIn": st.LoggedIn,
			"Phone": st.Phone, "PushName": st.PushName,
			"AccountID": accountID,
		})
	}
	if !db.IsPostgres() {
		renderStatus(store.LegacyAccountID)
		return
	}
	want := r.URL.Query().Get("account")
	if want != "" {
		a, err := db.GetAccount(want)
		if err != nil {
			writeStoreError(w, r, err)
			return
		}
		if !store.CanAccount(id.Role, id.Grants, a.ID, false) {
			WriteProblem(w, r, http.StatusNotFound, "Not Found", "resource not found")
			return
		}
		renderStatus(a.ID)
		return
	}
	accounts, err := db.AccountsByOrg(id.OrgID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	accounts = visibleAccounts(id, accounts)
	switch len(accounts) {
	case 0:
		h.Views.RenderPartial(w, "connect-dialog", map[string]any{
			"Connected": false, "LoggedIn": false, "NoAccounts": true,
		})
	case 1:
		renderStatus(accounts[0].ID)
	default:
		rows := make([]map[string]any, 0, len(accounts))
		for _, a := range accounts {
			st := h.WA.Status(a.ID)
			rows = append(rows, map[string]any{
				"ID": a.ID, "Label": a.Label, "Phone": a.Phone,
				"Status": a.Status, "Connected": st.Connected && st.LoggedIn,
			})
		}
		h.Views.RenderPartial(w, "connect-dialog", map[string]any{
			"Connected": false, "LoggedIn": false, "PickAccount": true, "Accounts": rows,
		})
	}
}

// fail maps WA errors to problem+json (or HTML for htmx).
func (h *Connection) fail(w http.ResponseWriter, r *http.Request, err error) {
	msg := err.Error()
	status := http.StatusBadGateway
	switch msg {
	case "already logged in":
		status = http.StatusConflict
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(status)
		_, _ = w.Write([]byte(`<div role="alert" class="alert alert-destructive"><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" x2="12" y1="8" y2="12"/><line x1="12" x2="12.01" y1="16" y2="16"/></svg><h5 class="alert-title">Request failed</h5><div class="alert-desc">` + template.HTMLEscapeString(msg) + `</div></div>`))
		return
	}
	WriteProblem(w, r, status, "WhatsApp error", msg)
}
