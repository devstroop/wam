package handlers

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/wa"
)

// Accounts serves WhatsApp number lifecycle (pairing state lives in Manager).
type Accounts struct {
	Store *store.DB
	Mgr   *wa.Manager
}

// ResolveSenderAccount validates/picks a sending account: explicit ids must
// exist in the org with caller use-access; "" resolves to the org's sole
// account (zero → pair first, many → caller must choose). Legacy SQLite
// returns "" (single implicit account).
func ResolveSenderAccount(db *store.DB, id middleware.Identity, want string) (string, error) {
	return resolveAccount(db, id, want, true)
}

// ResolveViewerAccount is ResolveSenderAccount for read paths (viewer ok).
func ResolveViewerAccount(db *store.DB, id middleware.Identity, want string) (string, error) {
	return resolveAccount(db, id, want, false)
}

// ResolveCampaignAccount validates/picks the campaign account: explicit ids
// must exist in the org with caller view-access (drafting is harmless;
// sending is gated separately at Start). "" resolves to the org's sole
// account (zero → pair first, many → caller must choose). Legacy SQLite
// returns "" (single implicit account).
func ResolveCampaignAccount(db *store.DB, id middleware.Identity, want string) (string, error) {
	return resolveAccount(db, id, want, false)
}

func resolveAccount(db *store.DB, id middleware.Identity, want string, needUse bool) (string, error) {
	if !db.IsPostgres() {
		return "", nil
	}
	if want != "" {
		if _, err := db.GetAccount(want); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return "", errors.New("unknown WhatsApp account")
			}
			return "", err
		}
		if !store.CanAccount(id.Role, id.Grants, want, needUse) {
			return "", errors.New("no access to that WhatsApp account")
		}
		return want, nil
	}
	accounts, err := db.AccountsByOrg(id.OrgID)
	if err != nil {
		return "", err
	}
	if d := store.ResolveDefaultAccount(accounts); d != "" {
		if !store.CanAccount(id.Role, id.Grants, d, needUse) {
			return "", errors.New("no account access (ask an admin for a grant)")
		}
		return d, nil
	}
	if len(accounts) == 0 {
		return "", errors.New("pair a WhatsApp account first")
	}
	return "", errors.New("choose a WhatsApp account")
}

// visibleAccounts filters org accounts by view access (admin sees all).
func visibleAccounts(id middleware.Identity, accounts []store.Account) []store.Account {
	if id.Role == store.RoleAdmin || id.Role == "" {
		return accounts
	}
	out := []store.Account{}
	for _, a := range accounts {
		if _, ok := id.Grants[a.ID]; ok {
			out = append(out, a)
		}
	}
	return out
}

// List returns viewable accounts with live status.
func (h *Accounts) List(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	if !db.IsPostgres() {
		WriteJSON(w, http.StatusOK, map[string]any{"data": []any{}})
		return
	}
	accounts, err := db.AccountsByOrg(id.OrgID)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	accounts = visibleAccounts(id, accounts)
	out := make([]map[string]any, 0, len(accounts))
	for _, a := range accounts {
		m := map[string]any{
			"id": a.ID, "label": a.Label, "phone": a.Phone,
			"status": a.Status, "lastSeenAt": a.LastSeenAt, "createdAt": a.CreatedAt,
		}
		if h.Mgr != nil {
			st := h.Mgr.Status(a.ID)
			m["connected"] = st.Connected
			m["loggedIn"] = st.LoggedIn
		}
		out = append(out, m)
	}
	WriteJSON(w, http.StatusOK, map[string]any{"data": out})
}

// Create adds a pending account row (pairing follows via QR/pair endpoints).
func (h *Accounts) Create(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, store.PermAccountsPair)
	if !ok {
		return
	}
	if !db.IsPostgres() {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "multi-account requires Postgres")
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Label = r.FormValue("label")
	}
	a, err := db.CreateAccount(id.OrgID, body.Label, id.UserID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Account added — pair it next"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusCreated)
		return
	}
	WriteJSON(w, http.StatusCreated, a)
}

// Get returns one account (view access).
func (h *Accounts) Get(w http.ResponseWriter, r *http.Request) {
	id, db, ok := Authorize(h.Store, w, r, "")
	if !ok {
		return
	}
	a, err := db.GetAccount(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !store.CanAccount(id.Role, id.Grants, a.ID, false) {
		WriteProblem(w, r, http.StatusNotFound, "Not Found", "resource not found")
		return
	}
	out := map[string]any{"id": a.ID, "label": a.Label, "phone": a.Phone, "status": a.Status,
		"lastSeenAt": a.LastSeenAt, "createdAt": a.CreatedAt}
	if h.Mgr != nil {
		st := h.Mgr.Status(a.ID)
		out["connected"] = st.Connected
		out["loggedIn"] = st.LoggedIn
	}
	WriteJSON(w, http.StatusOK, out)
}

// UpdateLabel renames an account (pair permission).
func (h *Accounts) UpdateLabel(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermAccountsPair)
	if !ok {
		return
	}
	var body struct {
		Label string `json:"label"`
	}
	if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
		return
	}
	a, err := db.UpdateAccountLabel(r.PathValue("id"), body.Label)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	WriteJSON(w, http.StatusOK, a)
}

// Delete removes an account row + device (pair permission).
func (h *Accounts) Delete(w http.ResponseWriter, r *http.Request) {
	_, db, ok := Authorize(h.Store, w, r, store.PermAccountsPair)
	if !ok {
		return
	}
	accountID := r.PathValue("id")
	a, err := db.GetAccount(accountID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if h.Mgr != nil {
		_ = h.Mgr.RemoveDevice(accountID, a.DeviceJID)
	}
	if err := db.DeleteAccount(accountID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// QR returns a pairing QR for one account (use access).
func (h *Accounts) QR(w http.ResponseWriter, r *http.Request) {
	h.pairGuard(w, r, func(a *store.Account) {
		ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
		defer cancel()
		dataURL, err := h.Mgr.QRCodePNG(ctx, a.ID, a.DeviceJID)
		if err != nil {
			WriteProblem(w, r, http.StatusBadGateway, "WhatsApp error", err.Error())
			return
		}
		if middleware.IsHTMX(r) {
			w.Header().Set("Content-Type", "text/html; charset=utf-8")
			_, _ = w.Write([]byte(`<img src="` + dataURL + `" alt="WhatsApp QR" style="max-width:256px;border-radius:8px" />`))
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"qr": dataURL, "expiresIn": 120})
	})
}

// Pair starts phone linking for one account (use access).
func (h *Accounts) Pair(w http.ResponseWriter, r *http.Request) {
	h.pairGuard(w, r, func(a *store.Account) {
		var body struct {
			Phone string `json:"phone"`
		}
		if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
			if err := json.NewDecoder(r.Body).Decode(&body); err != nil || body.Phone == "" {
				WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "phone is required (E.164)")
				return
			}
		} else {
			_ = r.ParseForm()
			body.Phone = r.FormValue("phone")
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		defer cancel()
		code, err := h.Mgr.PairPhone(ctx, a.ID, a.DeviceJID, body.Phone)
		if err != nil {
			WriteProblem(w, r, http.StatusBadGateway, "WhatsApp error", err.Error())
			return
		}
		WriteJSON(w, http.StatusOK, map[string]any{"code": code})
	})
}

// Logout wipes one account's device (use access).
func (h *Accounts) Logout(w http.ResponseWriter, r *http.Request) {
	h.pairGuard(w, r, func(a *store.Account) {
		if h.Mgr != nil {
			_ = h.Mgr.RemoveDevice(a.ID, a.DeviceJID)
		}
		_ = h.Store.MarkAccountLoggedOut(a.ID)
		w.WriteHeader(http.StatusNoContent)
	})
}

// pairGuard loads the account, checks use-access + manager presence.
func (h *Accounts) pairGuard(w http.ResponseWriter, r *http.Request, fn func(a *store.Account)) {
	id, db, ok := Authorize(h.Store, w, r, store.PermAccountsPair)
	if !ok {
		return
	}
	if h.Mgr == nil {
		WriteProblem(w, r, http.StatusServiceUnavailable, "Unavailable", "WhatsApp manager not configured")
		return
	}
	a, err := db.GetAccount(r.PathValue("id"))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if !store.CanAccount(id.Role, id.Grants, a.ID, true) {
		WriteProblem(w, r, http.StatusForbidden, "Forbidden", "no access to that WhatsApp account")
		return
	}
	fn(a)
}
