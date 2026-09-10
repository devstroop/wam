package server

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/store"
)

// formPost posts urlencoded form data with htmx headers.
func formPost(t *testing.T, h http.Handler, path, body, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	return serveReq(t, h, formReq("POST", path, body, cookie))
}

func formReq(method, path, body, cookie string) *http.Request {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	return req
}

func serveReq(t *testing.T, h http.Handler, req *http.Request) *httptest.ResponseRecorder {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestTeamSettingsCard covers the Team section: admin sees controls,
// members see a read-only list, invite/role flows work via forms.
func TestTeamSettingsCard(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-team-admin@example.com"
	memberEmail := "http-team-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	orgID := adminMs[0].OrgID

	hash, _ := auth.HashPassword("password123")
	memberUser, err := owner.CreateUser(memberEmail, "Member", hash)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	if _, err := owner.AddMembership(memberUser.ID, orgID, store.RoleUser); err != nil {
		t.Fatalf("join: %v", err)
	}
	_ = owner.VerifyUserEmail(memberUser.ID)
	rec, memberCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member login")

	// Admin sees invite form + role controls.
	rec, _ = doReq(t, h, "GET", "/settings", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin settings")
	adminBody := rec.Body.String()
	for _, want := range []string{"Team", "Invite member", "Grant number access", memberEmail, adminEmail} {
		if !strings.Contains(adminBody, want) {
			t.Fatalf("admin settings missing %q", want)
		}
	}

	// Member sees the list but no management controls.
	rec, _ = doReq(t, h, "GET", "/settings", "", memberCookie)
	mustStatus(t, rec, http.StatusOK, "member settings")
	memberBody := rec.Body.String()
	if !strings.Contains(memberBody, "Team") || !strings.Contains(memberBody, adminEmail) {
		t.Fatalf("member missing team list: %s", memberBody)
	}
	for _, notWant := range []string{"Invite member", "Grant number access", "hx-delete=\"/api/v1/members/"} {
		if strings.Contains(memberBody, notWant) {
			t.Fatalf("member sees admin control %q", notWant)
		}
	}

	// Invite a third user via form post (htmx toast, no refresh needed).
	rec = formPost(t, h, "/api/v1/members/invite",
		"email=http-invitee%40example.com&role=user", adminCookie)
	mustStatus(t, rec, http.StatusCreated, "invite form")
	invited, err := owner.GetUserByEmail("http-invitee@example.com")
	if err == nil {
		t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, invited.ID) })
	}
	_ = invited
	rec, _ = doReq(t, h, "GET", "/settings", "", adminCookie)
	if !strings.Contains(rec.Body.String(), "http-invitee@example.com") {
		t.Fatal("pending invite not listed")
	}

	// Role change via form-encoded PATCH.
	req := formReq("PATCH", "/api/v1/members/"+memberUser.ID, "role=admin", adminCookie)
	rec2 := serveReq(t, h, req)
	mustStatus(t, rec2, http.StatusOK, "promote form")
	m, _ := owner.GetMembership(memberUser.ID, orgID)
	if m.Role != store.RoleAdmin {
		t.Fatalf("role = %s", m.Role)
	}
	// Demote back so last-admin guard isn't tripped by other tests sharing orgs.
	req = formReq("PATCH", "/api/v1/members/"+memberUser.ID, "role=user", adminCookie)
	rec2 = serveReq(t, h, req)
	mustStatus(t, rec2, http.StatusOK, "demote form")
}

// TestTeamGrantsUI covers grant + revoke through the settings forms.
func TestTeamGrantsUI(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-grants-admin@example.com"
	memberEmail := "http-grants-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	orgID := adminMs[0].OrgID

	hash, _ := auth.HashPassword("password123")
	memberUser, _ := owner.CreateUser(memberEmail, "Member", hash)
	_, _ = owner.AddMembership(memberUser.ID, orgID, store.RoleUser)
	_ = owner.VerifyUserEmail(memberUser.ID)

	acc, err := owner.CreateAccount(orgID, "Grant Line", adminUser.ID)
	if err != nil {
		t.Fatalf("account: %v", err)
	}

	// Grant via form post.
	rec := formPost(t, h, "/api/v1/grants",
		fmt.Sprintf("user_id=%s&account_id=%s&role=viewer", memberUser.ID, acc.ID), adminCookie)
	mustStatus(t, rec, http.StatusOK, "grant form")
	rec, _ = doReq(t, h, "GET", "/settings", "", adminCookie)
	if !strings.Contains(rec.Body.String(), "Grant Line") {
		t.Fatalf("grant label not rendered: %s", rec.Body.String())
	}

	// Revoke via query-param DELETE (chip × button).
	rec, _ = doReq(t, h, "DELETE",
		fmt.Sprintf("/api/v1/grants?user_id=%s&account_id=%s", memberUser.ID, acc.ID), "", adminCookie)
	mustStatus(t, rec, http.StatusNoContent, "revoke query")
	rec, _ = doReq(t, h, "GET", "/settings", "", adminCookie)
	if strings.Contains(rec.Body.String(), "Grant Line · viewer") {
		t.Fatal("revoked grant still rendered")
	}
}
