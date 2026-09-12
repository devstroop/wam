package handlers

import (
	"net/http"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/middleware"
)

// Auth wires login/logout against the single-admin session.
type Auth struct {
	Session *auth.Session
	Login   func(w http.ResponseWriter, page string, data map[string]any)
}

// LoginPage renders the sign-in form (or a disabled-auth notice).
func (h *Auth) LoginPage(w http.ResponseWriter, r *http.Request) {
	h.Login(w, "auth/login.html", map[string]any{
		"Title":        "Login",
		"AuthDisabled": !h.Session.Enabled(),
		"Error":        r.URL.Query().Get("error") != "",
	})
}

// LoginSubmit checks the password; htmx partial errors stay on the form,
// normal posts redirect to /dashboard.
func (h *Auth) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	if err := r.ParseForm(); err != nil {
		http.Error(w, "bad form", http.StatusBadRequest)
		return
	}
	if !h.Session.CheckPassword(r.FormValue("password")) {
		if middleware.IsHTMX(r) {
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = w.Write([]byte(`<div role="alert" class="alert alert-destructive"><svg xmlns="http://www.w3.org/2000/svg" viewBox="0 0 24 24" fill="none" stroke="currentColor" stroke-width="2" stroke-linecap="round" stroke-linejoin="round"><circle cx="12" cy="12" r="10"/><line x1="12" x2="12" y1="8" y2="12"/><line x1="12" x2="12.01" y1="16" y2="16"/></svg><h5 class="alert-title">Authentication failed</h5><div class="alert-desc">Invalid password.</div></div>`))
			return
		}
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	h.Session.Issue(w, r)
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Redirect", "/dashboard")
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, "/dashboard", http.StatusSeeOther)
}

// Logout clears the session.
func (h *Auth) Logout(w http.ResponseWriter, r *http.Request) {
	h.Session.Clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}
