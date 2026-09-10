package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/handlers"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/views"
	"github.com/devstroop/wam/internal/wa"
)

type captureMailer struct {
	last map[string]string // to -> body
}

func (m *captureMailer) Send(to, subject, body string) error {
	if m.last == nil {
		m.last = map[string]string{}
	}
	m.last[to] = body
	return nil
}

var tokenRe = regexp.MustCompile(`token=([A-Za-z0-9_-]+)`)

func (m *captureMailer) tokenFor(t *testing.T, to string) string {
	t.Helper()
	match := tokenRe.FindStringSubmatch(m.last[to])
	if len(match) != 2 {
		t.Fatalf("no token mailed to %s", to)
	}
	return match[1]
}

// testMux wires the UMS HTTP surface with a capturing mailer.
func testMux(t *testing.T, app *store.DB) (http.Handler, *captureMailer) {
	t.Helper()
	sess := auth.New("", "test-secret-32-bytes-long-ok!!")
	mailer := &captureMailer{}
	v, err := views.New(map[string]any{})
	if err != nil {
		t.Fatalf("views: %v", err)
	}
	ums := &handlers.UMS{Store: app, Views: v, Session: sess, Mailer: mailer, BaseURL: "http://test"}
	contacts := &handlers.Contacts{Store: app, Views: v}
	camps := &handlers.Campaigns{Store: app, Views: v}
	mgr := wa.NewManager("", "", false, nil)
	accts := &handlers.Accounts{Store: app, Mgr: mgr}
	conn := &handlers.Connection{WA: mgr, Views: v, Store: app}
	mux := http.NewServeMux()
	mux.HandleFunc("POST /signup", ums.SignupSubmit)
	mux.HandleFunc("POST /login", ums.LoginSubmit)
	mux.HandleFunc("POST /logout", ums.LogoutSubmit)
	mux.HandleFunc("GET /verify", ums.VerifyPage)
	mux.HandleFunc("POST /forgot", ums.ForgotSubmit)
	mux.HandleFunc("POST /reset", ums.ResetSubmit)
	mux.HandleFunc("GET /invite/accept", ums.InviteAcceptPage)
	mux.HandleFunc("POST /invite/accept", ums.InviteAcceptSubmit)
	mux.HandleFunc("GET /api/v1/auth/me", ums.Me)
	mux.HandleFunc("POST /api/v1/auth/switch", ums.SwitchSubmit)
	mux.HandleFunc("GET /api/v1/members", ums.ListMembers)
	mux.HandleFunc("POST /api/v1/members/invite", ums.InviteMember)
	mux.HandleFunc("PATCH /api/v1/members/{user_id}", ums.UpdateMemberRole)
	mux.HandleFunc("DELETE /api/v1/members/{user_id}", ums.RemoveMember)
	mux.HandleFunc("GET /api/v1/contacts", contacts.List)
	mux.HandleFunc("POST /api/v1/contacts", contacts.Create)
	mux.HandleFunc("POST /api/v1/campaigns", camps.Create)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/start", camps.Start)
	mux.HandleFunc("POST /api/v1/campaigns/{id}/cancel", camps.Cancel)
	mux.HandleFunc("DELETE /api/v1/campaigns/{id}", camps.Delete)
	mux.HandleFunc("GET /api/v1/accounts", accts.List)
	mux.HandleFunc("POST /api/v1/accounts", accts.Create)
	mux.HandleFunc("GET /api/v1/accounts/{id}", accts.Get)
	mux.HandleFunc("PATCH /api/v1/accounts/{id}", accts.UpdateLabel)
	mux.HandleFunc("DELETE /api/v1/accounts/{id}", accts.Delete)
	mux.HandleFunc("POST /api/v1/accounts/{id}/qr", accts.QR)
	mux.HandleFunc("POST /api/v1/accounts/{id}/pair", accts.Pair)
	mux.HandleFunc("POST /api/v1/accounts/{id}/logout", accts.Logout)
	mux.HandleFunc("GET /api/v1/connection", conn.Status)
	mux.HandleFunc("GET /partials/connect-dialog", conn.Dialog)
	mux.HandleFunc("GET /partials/account-menu", ums.AccountMenu)
	mux.HandleFunc("POST /api/v1/grants", ums.SetGrantSubmit)
	mux.HandleFunc("GET /api/v1/grants", ums.ListGrants)
	billing := &handlers.Billing{Store: app}
	mux.HandleFunc("GET /api/v1/billing/plan", billing.Overview)
	mux.HandleFunc("GET /api/v1/billing/plans", billing.Plans)
	mux.HandleFunc("POST /api/v1/billing/subscription", billing.SetSubscription)
	mux.HandleFunc("GET /api/v1/billing/invoices", billing.Invoices)
	ops := &handlers.Ops{Store: app, Views: v}
	mux.HandleFunc("GET /api/v1/audit", ops.AuditList)
	mux.HandleFunc("GET /api/v1/keys", ops.ListKeys)
	mux.HandleFunc("POST /api/v1/keys", ops.CreateKey)
	mux.HandleFunc("DELETE /api/v1/keys/{id}", ops.RevokeKey)
	mux.HandleFunc("GET /api/v1/webhooks", ops.ListEndpoints)
	mux.HandleFunc("POST /api/v1/webhooks", ops.CreateEndpoint)
	mux.HandleFunc("DELETE /api/v1/webhooks/{id}", ops.DeleteEndpoint)
	mux.HandleFunc("GET /metrics", ops.Metrics)
	return sess.RequireUMS(app, nil)(mux), mailer
}

func doReq(t *testing.T, h http.Handler, method, path, body, cookie string) (*httptest.ResponseRecorder, string) {
	t.Helper()
	var reader *strings.Reader
	if body == "" {
		reader = strings.NewReader("")
	} else {
		reader = strings.NewReader(body)
	}
	req := httptest.NewRequest(method, path, reader)
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	setCookie := ""
	for _, c := range rec.Result().Cookies() {
		if c.Name == "wam_session" {
			setCookie = "wam_session=" + c.Value
		}
	}
	return rec, setCookie
}

func mustStatus(t *testing.T, rec *httptest.ResponseRecorder, want int, what string) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("%s: status %d, want %d: %s", what, rec.Code, want, rec.Body.String())
	}
}

// TestUMSHTTPEndToEnd drives signup→verify→login→team→403s over HTTP.
func TestUMSHTTPEndToEnd(t *testing.T) {
	ownerDSN := mustEnv(t, "WAM_DATABASE_URL")
	appDSN := mustEnv(t, "WAM_APP_DATABASE_URL")
	owner, err := store.OpenPostgres(ownerDSN)
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	app, err := store.OpenPostgresNoMigrate(appDSN)
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	h, mailer := testMux(t, app)

	adminEmail := "http-e2e-admin@example.com"
	memberEmail := "http-e2e-member@example.com"
	cleanup := func(email string) {
		t.Logf("cleanup start %s", email)
		u, err := owner.GetUserByEmail(email)
		if err != nil {
			t.Logf("cleanup lookup %s: %v", email, err)
			return
		}
		ms, _ := owner.MembershipsByUser(u.ID)
		if _, err := owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID); err != nil {
			t.Logf("cleanup user %s: %v", email, err)
		}
		for _, m := range ms {
			_, _ = owner.Exec(`DELETE FROM audit_logs WHERE org_id = ?`, m.OrgID)
			if n, _ := owner.MembersByOrg(m.OrgID); len(n) == 0 {
				if _, err := owner.Exec(`DELETE FROM orgs WHERE id = ? AND id != 'org_default'`, m.OrgID); err != nil {
					t.Logf("cleanup org %s: %v", m.OrgID, err)
				}
			}
		}
		// Belt and suspenders: nothing with our addresses may survive.
		if _, err := owner.Exec(`DELETE FROM users WHERE email = ?`, email); err != nil {
			t.Logf("cleanup user by email %s: %v", email, err)
		}
	}
	t.Cleanup(func() { cleanup(adminEmail); cleanup(memberEmail) })

	// 1. Signup admin (JSON) → 201, unverified.
	rec, _ := doReq(t, h, "POST", "/signup",
		fmt.Sprintf(`{"email":%q,"name":"E2E Admin","password":"password123"}`, adminEmail), "")
	mustStatus(t, rec, http.StatusCreated, "signup")
	verifyToken := mailer.tokenFor(t, adminEmail)

	// 2. Login before verify → 403.
	rec, _ = doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, adminEmail), "")
	mustStatus(t, rec, http.StatusForbidden, "login-unverified")

	// 3. Verify → auto-login (302 + cookie).
	rec, adminCookie := doReq(t, h, "GET", "/verify?token="+verifyToken, "", "")
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("verify: status %d: %s", rec.Code, rec.Body.String())
	}
	if adminCookie == "" {
		t.Fatal("verify did not issue session")
	}

	// 4. Me → admin.
	rec, _ = doReq(t, h, "GET", "/api/v1/auth/me", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "me")
	var me struct {
		Role  string `json:"role"`
		OrgID string `json:"orgId"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil || me.Role != "admin" || me.OrgID == "" {
		t.Fatalf("me = %s err=%v", rec.Body.String(), err)
	}

	// 5. Last-admin self-demote → 409. Need own user id.
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	rec, _ = doReq(t, h, "PATCH", "/api/v1/members/"+adminUser.ID, `{"role":"user"}`, adminCookie)
	mustStatus(t, rec, http.StatusConflict, "last-admin demote")

	// 6. Invite member → 201 + mailed token.
	rec, _ = doReq(t, h, "POST", "/api/v1/members/invite",
		fmt.Sprintf(`{"email":%q,"role":"user"}`, memberEmail), adminCookie)
	mustStatus(t, rec, http.StatusCreated, "invite")
	inviteToken := mailer.tokenFor(t, memberEmail)

	// 7. Member signup with invite → auto-joined + logged in.
	rec, memberCookie := doReq(t, h, "POST", "/signup",
		fmt.Sprintf(`{"email":%q,"name":"E2E Member","password":"password123","invite":%q}`, memberEmail, inviteToken), "")
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusOK {
		t.Fatalf("invite signup: status %d: %s", rec.Code, rec.Body.String())
	}
	if memberCookie == "" {
		t.Fatal("invite signup did not issue session")
	}
	rec, _ = doReq(t, h, "GET", "/api/v1/auth/me", "", memberCookie)
	mustStatus(t, rec, http.StatusOK, "member me")
	var memberMe struct {
		Role  string `json:"role"`
		OrgID string `json:"orgId"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&memberMe)
	if memberMe.Role != "user" || memberMe.OrgID != me.OrgID {
		t.Fatalf("member me = %+v, want user in %s", memberMe, me.OrgID)
	}

	// 8. Member RBAC: invites/members → 403, contacts read → 200.
	rec, _ = doReq(t, h, "GET", "/api/v1/members", "", memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "member list members")
	rec, _ = doReq(t, h, "POST", "/api/v1/members/invite", `{"email":"x@y.zz","role":"user"}`, memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "member invite")
	rec, _ = doReq(t, h, "GET", "/api/v1/contacts", "", memberCookie)
	mustStatus(t, rec, http.StatusOK, "member list contacts")

	// 9. Member cannot demote admin (403 before last-admin logic).
	rec, _ = doReq(t, h, "PATCH", "/api/v1/members/"+adminUser.ID, `{"role":"user"}`, memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "member demote admin")

	// 10. Admin promotes member → member is admin; admin demotes self → ok.
	memberUser, _ := owner.GetUserByEmail(memberEmail)
	rec, _ = doReq(t, h, "PATCH", "/api/v1/members/"+memberUser.ID, `{"role":"admin"}`, adminCookie)
	mustStatus(t, rec, http.StatusOK, "promote member")
	// member cookie holds stale role (user) → re-login for fresh grants.
	rec, memberCookie = doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member re-login")
	rec, _ = doReq(t, h, "PATCH", "/api/v1/members/"+adminUser.ID, `{"role":"user"}`, adminCookie)
	mustStatus(t, rec, http.StatusOK, "demote with backup admin")

	// 11. Forgot/reset via HTTP with captured token.
	rec, _ = doReq(t, h, "POST", "/forgot",
		fmt.Sprintf(`{"email":%q}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "forgot")
	resetToken := mailer.tokenFor(t, memberEmail)
	rec, _ = doReq(t, h, "POST", "/reset",
		fmt.Sprintf(`{"token":%q,"password":"newpassword123"}`, resetToken), "")
	mustStatus(t, rec, http.StatusOK, "reset")
	// Old session revoked by reset.
	rec, _ = doReq(t, h, "GET", "/api/v1/auth/me", "", memberCookie)
	mustStatus(t, rec, http.StatusUnauthorized, "old session after reset")
	// New password works.
	rec, _ = doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"newpassword123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "login with new password")

	// 12. Unauthenticated API → 401.
	rec, _ = doReq(t, h, "GET", "/api/v1/auth/me", "", "")
	mustStatus(t, rec, http.StatusUnauthorized, "anon me")
}

func mustEnv(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skip(key + " unset")
	}
	return v
}
