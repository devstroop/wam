package handlers

import (
	"database/sql"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/mail"
	"github.com/devstroop/wam/internal/middleware"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
)

// UMS serves identity flows (signup/login/verify/reset/invites) and team
// management. Public form pages use the pool handle with token-gated
// predicates; authenticated APIs resolve the org-scoped handle via gate.
type UMS struct {
	Store   *store.DB // pool handle (app role); session/token tables are unscoped by design
	Views   *views.Views
	Session *auth.Session
	Mailer  mail.Mailer
	BaseURL string
}

// gate resolves the scoped handle + checks perm. Legacy SQLite requests carry
// no identity and pass (single-admin already authorized at the middleware).
func (h *UMS) gate(w http.ResponseWriter, r *http.Request, perm string) (middleware.Identity, *store.DB, bool) {
	db := middleware.ScopedDB(r, h.Store)
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
	return id, db, true
}

func (h *UMS) wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Content-Type"), "application/json") ||
		strings.HasPrefix(r.URL.Path, "/api/")
}

// issueSession creates the server-side row and sets the cookie.
func (h *UMS) issueSession(w http.ResponseWriter, r *http.Request, userID, orgID string) error {
	raw := h.Session.Issue(w, r)
	_, err := h.Store.CreateSession(userID, orgID, auth.HashToken(raw), time.Now().Add(30*24*time.Hour))
	if err != nil {
		h.Session.Clear(w)
	}
	return err
}

// currentUser loads the logged-in user on public pages (best effort).
func (h *UMS) currentUser(r *http.Request) (*store.User, bool) {
	raw, ok := h.Session.CookieValue(r)
	if !ok || !h.Session.Valid(r) {
		return nil, false
	}
	srow, err := h.Store.GetSessionByTokenHash(auth.HashToken(raw))
	if err != nil {
		return nil, false
	}
	user, err := h.Store.GetUser(srow.UserID)
	if err != nil {
		return nil, false
	}
	return user, true
}

// --- Signup ---

// SignupPage renders the registration form (?invite=token joins that org).
func (h *UMS) SignupPage(w http.ResponseWriter, r *http.Request) {
	h.Views.RenderPage(w, "signup.html", map[string]any{
		"Title":  "Sign up",
		"Invite": strings.TrimSpace(r.URL.Query().Get("invite")),
		"Error":  r.URL.Query().Get("error"),
	})
}

// SignupSubmit registers a user: invite token joins that org (verified by
// token possession), otherwise a new org is created and the user admins it.
func (h *UMS) SignupSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Name     string `json:"name"`
		Password string `json:"password"`
		Invite   string `json:"invite"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Email, body.Name, body.Password, body.Invite =
			r.FormValue("email"), r.FormValue("name"), r.FormValue("password"), r.FormValue("invite")
	}
	fail := func(detail string) {
		if h.wantsJSON(r) {
			WriteProblem(w, r, http.StatusBadRequest, "Signup failed", detail)
			return
		}
		http.Redirect(w, r, "/signup?error=1", http.StatusSeeOther)
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		fail(err.Error())
		return
	}
	user, err := h.Store.CreateUser(body.Email, body.Name, hash)
	if err != nil {
		fail(err.Error())
		return
	}
	// Invite path: join invited org, verified by token possession.
	if strings.TrimSpace(body.Invite) != "" {
		if inv, err := h.Store.GetInviteByTokenHash(auth.HashToken(body.Invite)); err == nil && strings.EqualFold(inv.Email, user.Email) {
			_ = h.Store.AcceptInvite(inv.ID)
			_ = h.Store.VerifyUserEmail(user.ID)
			if _, err := h.Store.AddMembership(user.ID, inv.OrgID, inv.Role); err == nil {
				if err := h.issueSession(w, r, user.ID, inv.OrgID); err == nil {
					h.redirectAuthed(w, r, "/dashboard")
					return
				}
			}
		}
		fail("invalid or mismatched invite")
		return
	}
	orgName := strings.TrimSpace(body.Name)
	if orgName == "" {
		orgName = strings.Split(user.Email, "@")[0]
	}
	org, err := h.Store.CreateOrg(orgName + "'s workspace")
	if err != nil {
		fail(err.Error())
		return
	}
	if _, err := h.Store.AddMembership(user.ID, org.ID, store.RoleAdmin); err != nil {
		fail(err.Error())
		return
	}
	if _, _, err := h.sendVerification(user); err != nil {
		fail(err.Error())
		return
	}
	if h.wantsJSON(r) {
		WriteJSON(w, http.StatusCreated, map[string]any{"id": user.ID, "email": user.Email})
		return
	}
	http.Redirect(w, r, "/verify?sent=1", http.StatusSeeOther)
}

func (h *UMS) sendVerification(user *store.User) (string, string, error) {
	raw, digest, err := auth.NewToken()
	if err != nil {
		return "", "", err
	}
	if err := h.Store.CreateEmailVerification(user.ID, digest, time.Now().Add(24*time.Hour)); err != nil {
		return "", "", err
	}
	if err := h.Mailer.Send(user.Email, "Verify your WAM account", mail.VerifyBody(h.BaseURL, raw)); err != nil {
		return "", "", err
	}
	return raw, digest, nil
}

func (h *UMS) redirectAuthed(w http.ResponseWriter, r *http.Request, to string) {
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Redirect", to)
		w.WriteHeader(http.StatusOK)
		return
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// --- Login / logout ---

// LoginPage renders the UMS sign-in form.
func (h *UMS) LoginPage(w http.ResponseWriter, r *http.Request) {
	h.Views.RenderPage(w, "login.html", map[string]any{
		"Title":     "Login",
		"UMS":       true,
		"Error":     r.URL.Query().Get("error"),
		"ResendMsg": r.URL.Query().Get("resend"),
	})
}

// LoginSubmit checks email+password, requires verified email + membership.
func (h *UMS) LoginSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email    string `json:"email"`
		Password string `json:"password"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Email, body.Password = r.FormValue("email"), r.FormValue("password")
	}
	fail := func() {
		if h.wantsJSON(r) {
			WriteProblem(w, r, http.StatusUnauthorized, "Login failed", "invalid credentials")
			return
		}
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
	}
	user, err := h.Store.GetUserByEmail(body.Email)
	if err != nil {
		fail()
		return
	}
	hash, err := h.Store.UserPasswordHash(user.ID)
	if err != nil || !auth.CheckPasswordHash(hash, body.Password) {
		fail()
		return
	}
	if strings.TrimSpace(user.EmailVerifiedAt) == "" {
		if h.wantsJSON(r) {
			WriteProblem(w, r, http.StatusForbidden, "Email unverified", "check your inbox for the verification link")
			return
		}
		http.Redirect(w, r, "/login?error=1", http.StatusSeeOther)
		return
	}
	memberships, err := h.Store.MembershipsByUser(user.ID)
	if err != nil || len(memberships) == 0 {
		fail()
		return
	}
	if err := h.issueSession(w, r, user.ID, memberships[0].OrgID); err != nil {
		fail()
		return
	}
	if h.wantsJSON(r) {
		WriteJSON(w, http.StatusOK, map[string]any{"id": user.ID, "email": user.Email, "orgId": memberships[0].OrgID})
		return
	}
	h.redirectAuthed(w, r, "/dashboard")
}

// LogoutSubmit revokes the server-side session and clears the cookie.
func (h *UMS) LogoutSubmit(w http.ResponseWriter, r *http.Request) {
	if raw, ok := h.Session.CookieValue(r); ok {
		_ = h.Store.RevokeSession(auth.HashToken(raw))
	}
	h.Session.Clear(w)
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Verify / forgot / reset ---

// VerifyPage consumes the token and auto-logs in on success.
func (h *UMS) VerifyPage(w http.ResponseWriter, r *http.Request) {
	if token := strings.TrimSpace(r.URL.Query().Get("token")); token != "" {
		userID, err := h.Store.ConsumeEmailVerification(auth.HashToken(token))
		if err != nil {
			http.Redirect(w, r, "/verify?error=1", http.StatusSeeOther)
			return
		}
		if user, err := h.Store.GetUser(userID); err == nil {
			if ms, err := h.Store.MembershipsByUser(user.ID); err == nil && len(ms) > 0 {
				if err := h.issueSession(w, r, user.ID, ms[0].OrgID); err == nil {
					h.redirectAuthed(w, r, "/dashboard")
					return
				}
			}
		}
		h.Views.RenderPage(w, "verify.html", map[string]any{"Title": "Verified", "Done": true})
		return
	}
	h.Views.RenderPage(w, "verify.html", map[string]any{
		"Title": "Verify email",
		"Sent":  r.URL.Query().Get("sent") != "",
		"Error": r.URL.Query().Get("error") != "",
	})
}

// ForgotPage renders the reset-request form.
func (h *UMS) ForgotPage(w http.ResponseWriter, r *http.Request) {
	h.Views.RenderPage(w, "forgot.html", map[string]any{
		"Title": "Forgot password",
		"Sent":  r.URL.Query().Get("sent") != "",
	})
}

// ForgotSubmit always succeeds (no account enumeration).
func (h *UMS) ForgotSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Email string `json:"email"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		_ = json.NewDecoder(r.Body).Decode(&body)
	} else {
		_ = r.ParseForm()
		body.Email = r.FormValue("email")
	}
	if user, err := h.Store.GetUserByEmail(body.Email); err == nil {
		raw, digest, err := auth.NewToken()
		if err == nil {
			if err := h.Store.CreatePasswordReset(user.ID, digest, time.Now().Add(time.Hour)); err == nil {
				_ = h.Mailer.Send(user.Email, "Reset your WAM password", mail.ResetBody(h.BaseURL, raw))
			}
		}
	}
	if h.wantsJSON(r) {
		WriteJSON(w, http.StatusOK, map[string]any{"sent": true})
		return
	}
	http.Redirect(w, r, "/forgot?sent=1", http.StatusSeeOther)
}

// ResetPage renders the new-password form (token in query).
func (h *UMS) ResetPage(w http.ResponseWriter, r *http.Request) {
	h.Views.RenderPage(w, "reset.html", map[string]any{
		"Title": "Reset password",
		"Token": strings.TrimSpace(r.URL.Query().Get("token")),
		"Error": r.URL.Query().Get("error") != "",
	})
}

// ResetSubmit consumes the token, sets the password, revokes all sessions.
func (h *UMS) ResetSubmit(w http.ResponseWriter, r *http.Request) {
	var body struct {
		Token    string `json:"token"`
		Password string `json:"password"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Token, body.Password = r.FormValue("token"), r.FormValue("password")
	}
	fail := func() {
		if h.wantsJSON(r) {
			WriteProblem(w, r, http.StatusBadRequest, "Reset failed", "invalid or expired token")
			return
		}
		http.Redirect(w, r, "/reset?error=1&token="+body.Token, http.StatusSeeOther)
	}
	userID, err := h.Store.ConsumePasswordReset(auth.HashToken(body.Token))
	if err != nil {
		fail()
		return
	}
	hash, err := auth.HashPassword(body.Password)
	if err != nil {
		fail()
		return
	}
	if err := h.Store.SetUserPassword(userID, hash); err != nil {
		fail()
		return
	}
	_ = h.Store.RevokeUserSessions(userID)
	if h.wantsJSON(r) {
		WriteJSON(w, http.StatusOK, map[string]any{"reset": true})
		return
	}
	http.Redirect(w, r, "/login", http.StatusSeeOther)
}

// --- Invite accept ---

// InviteAcceptPage shows invitation details (?token=).
func (h *UMS) InviteAcceptPage(w http.ResponseWriter, r *http.Request) {
	token := strings.TrimSpace(r.URL.Query().Get("token"))
	inv, err := h.Store.GetInviteByTokenHash(auth.HashToken(token))
	if err != nil {
		h.Views.RenderPage(w, "invite-accept.html", map[string]any{"Title": "Invite", "Error": true})
		return
	}
	orgName := inv.OrgName
	if orgName == "" {
		orgName = inv.OrgID
	}
	user, loggedIn := h.currentUser(r)
	h.Views.RenderPage(w, "invite-accept.html", map[string]any{
		"Title": "Accept invite", "Invite": inv, "OrgName": orgName,
		"Token": token, "LoggedIn": loggedIn, "User": user,
		"Matches": loggedIn && user != nil && strings.EqualFold(user.Email, inv.Email),
	})
}

// InviteAcceptSubmit joins the org (logged in as the invited email).
func (h *UMS) InviteAcceptSubmit(w http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	token := strings.TrimSpace(r.FormValue("token"))
	inv, err := h.Store.GetInviteByTokenHash(auth.HashToken(token))
	if err != nil {
		http.Redirect(w, r, "/invite/accept?error=1", http.StatusSeeOther)
		return
	}
	user, loggedIn := h.currentUser(r)
	if !loggedIn || user == nil || !strings.EqualFold(user.Email, inv.Email) {
		http.Redirect(w, r, "/signup?invite="+token, http.StatusSeeOther)
		return
	}
	_ = h.Store.AcceptInvite(inv.ID)
	_ = h.Store.VerifyUserEmail(user.ID)
	if _, err := h.Store.AddMembership(user.ID, inv.OrgID, inv.Role); err != nil &&
		!strings.Contains(err.Error(), "already a member") {
		http.Redirect(w, r, "/invite/accept?error=1", http.StatusSeeOther)
		return
	}
	h.redirectAuthed(w, r, "/dashboard")
}

// orgNameFor resolves the workspace name via the scoped handle (fail-closed
// RLS hides orgs from unscoped handles).
func orgNameFor(db *store.DB, orgID string) string {
	if o, err := db.GetOrg(orgID); err == nil {
		return o.Name
	}
	return ""
}

// --- Self / switch ---

// Me returns the caller's identity + orgs.
func (h *UMS) Me(w http.ResponseWriter, r *http.Request) {
	id, db, ok := h.gate(w, r, "")
	if !ok {
		return
	}
	orgs := []map[string]any{}
	if ms, err := h.Store.MembershipsByUser(id.UserID); err == nil {
		for _, m := range ms {
			name := m.OrgID
			if o, err := db.GetOrg(m.OrgID); err == nil {
				name = o.Name
			}
			orgs = append(orgs, map[string]any{"id": m.OrgID, "name": name, "role": m.Role})
		}
	}
	WriteJSON(w, http.StatusOK, map[string]any{
		"id": id.UserID, "email": id.Email, "name": id.Name,
		"orgId": id.OrgID, "role": id.Role, "orgs": orgs,
	})
}

// SwitchSubmit moves the session to another org the user belongs to.
func (h *UMS) SwitchSubmit(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.gate(w, r, "")
	if !ok {
		return
	}
	var body struct {
		OrgID string `json:"org_id"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.OrgID = r.FormValue("org_id")
	}
	if _, err := h.Store.GetMembership(id.UserID, body.OrgID); err != nil {
		WriteProblem(w, r, http.StatusForbidden, "Forbidden", "not a member of that organization")
		return
	}
	raw, ok := h.Session.CookieValue(r)
	if !ok {
		WriteProblem(w, r, http.StatusUnauthorized, "Unauthorized", "login required")
		return
	}
	if err := h.Store.SetSessionOrg(auth.HashToken(raw), body.OrgID); err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"orgId": body.OrgID})
}

// --- Team (admin: members:manage) ---

// ListMembers returns members + pending invites.
func (h *UMS) ListMembers(w http.ResponseWriter, r *http.Request) {
	id, db, ok := h.gate(w, r, store.PermMembersManage)
	if !ok {
		return
	}
	_ = db
	members, err := h.Store.MembersByOrg(id.OrgID)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	invites, err := h.Store.InvitesByOrg(id.OrgID)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"members": members, "invites": invites})
}

// InviteMember creates an invitation and mails the link.
func (h *UMS) InviteMember(w http.ResponseWriter, r *http.Request) {
	id, db, ok := h.gate(w, r, store.PermMembersManage)
	if !ok {
		return
	}
	var body struct {
		Email string `json:"email"`
		Role  string `json:"role"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Email, body.Role = r.FormValue("email"), r.FormValue("role")
	}
	if body.Role == "" {
		body.Role = store.RoleUser
	}
	if db.IsPostgres() {
		if err := db.CheckQuota(id.OrgID, "member"); err != nil {
			writeStoreError(w, r, err)
			return
		}
	}
	raw, digest, err := auth.NewToken()
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	inv, err := h.Store.CreateInvite(id.OrgID, orgNameFor(db, id.OrgID), body.Email, body.Role, digest, id.UserID, time.Now().Add(7*24*time.Hour))
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	orgName := inv.OrgName
	if orgName == "" {
		orgName = id.OrgID
	}
	_ = h.Mailer.Send(inv.Email, "You're invited to "+orgName+" on WAM", mail.InviteBody(h.BaseURL, orgName, raw))
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Invite sent"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusCreated)
		return
	}
	WriteJSON(w, http.StatusCreated, inv)
}

// UpdateMemberRole changes a member's role (last-admin guard in store).
func (h *UMS) UpdateMemberRole(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.gate(w, r, store.PermMembersManage)
	if !ok {
		return
	}
	userID := r.PathValue("user_id")
	var body struct {
		Role string `json:"role"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.Role = r.FormValue("role")
	}
	m, err := h.Store.SetMemberRole(userID, id.OrgID, body.Role)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	_ = h.Store.RevokeUserOrgSessions(userID, id.OrgID) // force re-login with new role
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Role updated"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	WriteJSON(w, http.StatusOK, m)
}

// RemoveMember removes a member (last-admin guard) and revokes sessions.
func (h *UMS) RemoveMember(w http.ResponseWriter, r *http.Request) {
	id, _, ok := h.gate(w, r, store.PermMembersManage)
	if !ok {
		return
	}
	userID := r.PathValue("user_id")
	if userID == id.UserID {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "cannot remove yourself")
		return
	}
	if err := h.Store.RemoveMembership(userID, id.OrgID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	_ = h.Store.RevokeUserOrgSessions(userID, id.OrgID)
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Member removed"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

// --- Grants (admin: members:manage; enforced per-account in multi-account) ---

// ListGrants returns a member's account grants (?user_id=).
func (h *UMS) ListGrants(w http.ResponseWriter, r *http.Request) {
	id, db, ok := h.gate(w, r, store.PermMembersManage)
	if !ok {
		return
	}
	userID := strings.TrimSpace(r.URL.Query().Get("user_id"))
	if userID == "" {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "user_id required")
		return
	}
	if _, err := db.GetMembership(userID, id.OrgID); err != nil {
		if err == sql.ErrNoRows {
			WriteProblem(w, r, http.StatusNotFound, "Not Found", "not a member")
			return
		}
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	grants, err := db.GrantsForUser(userID, id.OrgID)
	if err != nil {
		WriteProblem(w, r, http.StatusInternalServerError, "Store error", err.Error())
		return
	}
	WriteJSON(w, http.StatusOK, map[string]any{"grants": grants})
}

// SetGrantSubmit grants a member access to an account.
func (h *UMS) SetGrantSubmit(w http.ResponseWriter, r *http.Request) {
	id, db, ok := h.gate(w, r, store.PermMembersManage)
	if !ok {
		return
	}
	var body struct {
		UserID    string `json:"user_id"`
		AccountID string `json:"account_id"`
		Role      string `json:"role"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.UserID, body.AccountID, body.Role = r.FormValue("user_id"), r.FormValue("account_id"), r.FormValue("role")
	}
	if _, err := db.GetMembership(body.UserID, id.OrgID); err != nil {
		WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "grantee must be an org member")
		return
	}
	g, err := db.SetGrant(body.UserID, body.AccountID, id.OrgID, body.Role, id.UserID)
	if err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Access granted"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	WriteJSON(w, http.StatusOK, g)
}

// RemoveGrantSubmit revokes account access.
func (h *UMS) RemoveGrantSubmit(w http.ResponseWriter, r *http.Request) {
	_, db, ok := h.gate(w, r, store.PermMembersManage)
	if !ok {
		return
	}
	var body struct {
		UserID    string `json:"user_id"`
		AccountID string `json:"account_id"`
	}
	if strings.Contains(r.Header.Get("Content-Type"), "application/json") {
		if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
			WriteProblem(w, r, http.StatusBadRequest, "Bad Request", "invalid JSON body")
			return
		}
	} else {
		_ = r.ParseForm()
		body.UserID, body.AccountID = r.FormValue("user_id"), r.FormValue("account_id")
	}
	if err := db.RemoveGrant(body.UserID, body.AccountID); err != nil {
		writeStoreError(w, r, err)
		return
	}
	if middleware.IsHTMX(r) {
		w.Header().Set("HX-Trigger", `{"toast":"Access revoked"}`)
		w.Header().Set("HX-Refresh", "true")
		w.WriteHeader(http.StatusOK)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}
