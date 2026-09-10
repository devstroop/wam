package handlers

import (
	"context"
	"encoding/json"
	"html/template"
	"net/http"
	"time"

	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
)

// Connection serves the WhatsApp lifecycle.
// JSON for API clients; HTML fragments when HX-Request is present.
type Connection struct {
	WA    *wa.Service
	Views *views.Views
}

// Status returns live connection state.
func (h *Connection) Status(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := Authorize(nil, w, r, store.PermAccountsView); !ok {
		return
	}
	WriteJSON(w, http.StatusOK, h.WA.Status())
}

// QR returns a pairing QR: JSON {qr, expiresIn} for API, <img> for htmx.
// Times out after ~45s waiting for the first code event.
func (h *Connection) QR(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := Authorize(nil, w, r, store.PermAccountsPair); !ok {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 45*time.Second)
	defer cancel()
	dataURL, err := h.WA.QRCodePNG(ctx)
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
	if _, _, ok := Authorize(nil, w, r, store.PermAccountsPair); !ok {
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
	code, err := h.WA.PairPhone(ctx, phone)
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
	if _, _, ok := Authorize(nil, w, r, store.PermAccountsPair); !ok {
		return
	}
	if err := h.WA.Logout(); err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Logout failed", err.Error())
		return
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
	if _, _, ok := Authorize(nil, w, r, store.PermAccountsView); !ok {
		return
	}
	st := h.WA.Status()
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

// Sidebar renders the compose-style connection block below the logo
// (polled every 15s). Offline → full-width "Link a device" button;
// linking → amber variant; connected → account row. All open the dialog.
func (h *Connection) Sidebar(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := Authorize(nil, w, r, store.PermAccountsView); !ok {
		return
	}
	st := h.WA.Status()
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	const waSVG = `<svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><path d="M3 21l1.65-3.8a9 9 0 1 1 3.4 2.9L3 21"/><path d="M9.5 9a.5.5 0 0 0 1 0V9a.5.5 0 0 0-1 0v1a5 5 0 0 0 5 5h1a.5.5 0 0 0 0-1h-1a.5.5 0 0 0 0 1"/></svg>`
	var out string
	switch {
	case st.Connected && st.LoggedIn:
		phone := template.HTMLEscapeString(st.Phone)
		out = `<button type="button" class="account-chip" onclick="openConnectDialog()" title="WhatsApp connected">` +
			`<span class="account-chip-icon">` + waSVG + `</span>` +
			`<span class="sidebar-text account-meta"><span class="account-phone">` + phone + `</span><span class="account-sub">Connected</span></span>` +
			`<span class="badge-dot is-live"></span></button>`
	case st.Connected:
		out = `<button type="button" class="link-device-btn is-linking" onclick="openConnectDialog()" title="Linking WhatsApp…">` +
			`<span class="link-device-icon">` + waSVG + `</span><span class="sidebar-text">Linking…</span><span class="badge-dot"></span></button>`
	default:
		out = `<button type="button" class="link-device-btn is-off" onclick="openConnectDialog()" title="Link a WhatsApp device">` +
			`<span class="link-device-icon">` + waSVG + `</span><span class="sidebar-text">Link a device</span><span class="badge-dot"></span></button>`
	}
	_, _ = w.Write([]byte(out))
}

// Dialog renders the connect dialog body: rich status + disconnect when
// linked, otherwise Scan QR / Pair tabs.
func (h *Connection) Dialog(w http.ResponseWriter, r *http.Request) {
	if _, _, ok := Authorize(nil, w, r, store.PermAccountsView); !ok {
		return
	}
	st := h.WA.Status()
	if h.Views == nil {
		http.Error(w, "views unavailable", http.StatusInternalServerError)
		return
	}
	h.Views.RenderPartial(w, "connect-dialog", map[string]any{
		"Connected": st.Connected, "LoggedIn": st.LoggedIn,
		"Phone": st.Phone, "PushName": st.PushName,
	})
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
