package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/store"
)

// TestAccountsHTTPAccess covers account CRUD + grant gating over HTTP.
// Live pairing (QR/pair) needs a real device and stays manual.
func TestAccountsHTTPAccess(t *testing.T) {
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
	h, _ := testMux(t, app)

	adminEmail := "http-acct-admin@example.com"
	memberEmail := "http-acct-member@example.com"
	t.Cleanup(func() {
		for _, email := range []string{adminEmail, memberEmail} {
			u, err := owner.GetUserByEmail(email)
			if err != nil {
				continue
			}
			ms, _ := owner.MembershipsByUser(u.ID)
			_, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID)
			for _, m := range ms {
				if n, _ := owner.MembersByOrg(m.OrgID); len(n) == 0 {
					_, _ = owner.Exec(`DELETE FROM orgs WHERE id = ? AND id != 'org_default'`, m.OrgID)
				}
			}
			_, _ = owner.Exec(`DELETE FROM users WHERE email = ?`, email)
		}
	})

	// Admin via real signup (own org).
	rec, _ := doReq(t, h, "POST", "/signup",
		fmt.Sprintf(`{"email":%q,"name":"Acct","password":"password123"}`, adminEmail), "")
	mustStatus(t, rec, http.StatusCreated, "admin signup")
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	_ = owner.VerifyUserEmail(adminUser.ID)
	rec, adminCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, adminEmail), "")
	mustStatus(t, rec, http.StatusOK, "admin login")
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	orgID := adminMs[0].OrgID

	// Member joins the same org (store-level; invite HTTP flow is in e2e).
	hash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	memberUser, err := owner.CreateUser(memberEmail, "Member", hash)
	if err != nil {
		t.Fatalf("member user: %v", err)
	}
	if _, err := owner.AddMembership(memberUser.ID, orgID, store.RoleUser); err != nil {
		t.Fatalf("member join: %v", err)
	}
	if err := owner.VerifyUserEmail(memberUser.ID); err != nil {
		t.Fatalf("verify: %v", err)
	}
	rec, memberCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member login")

	decodeID := func(rec *httptest.ResponseRecorder) string {
		var v struct {
			ID string `json:"id"`
		}
		if err := json.NewDecoder(rec.Body).Decode(&v); err != nil || v.ID == "" {
			t.Fatalf("decode id: %v %s", err, rec.Body.String())
		}
		return v.ID
	}

	// 1. Admin creates two accounts.
	rec, _ = doReq(t, h, "POST", "/api/v1/accounts", `{"label":"Sales"}`, adminCookie)
	mustStatus(t, rec, http.StatusCreated, "create A")
	acctA := decodeID(rec)
	rec, _ = doReq(t, h, "POST", "/api/v1/accounts", `{"label":"Support"}`, adminCookie)
	mustStatus(t, rec, http.StatusCreated, "create B")
	acctB := decodeID(rec)

	// 2. Member sees none, gets 404 on direct fetch.
	rec, _ = doReq(t, h, "GET", "/api/v1/accounts", "", memberCookie)
	mustStatus(t, rec, http.StatusOK, "member list")
	var list struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&list)
	if len(list.Data) != 0 {
		t.Fatalf("member sees %d accounts", len(list.Data))
	}
	rec, _ = doReq(t, h, "GET", "/api/v1/accounts/"+acctA, "", memberCookie)
	mustStatus(t, rec, http.StatusNotFound, "member get ungranted")

	// 3. Admin grants viewer on A: member sees A, but QR (use) → 403.
	rec, _ = doReq(t, h, "POST", "/api/v1/grants",
		fmt.Sprintf(`{"user_id":%q,"account_id":%q,"role":"viewer"}`, memberUser.ID, acctA), adminCookie)
	mustStatus(t, rec, http.StatusOK, "grant viewer")
	rec, _ = doReq(t, h, "GET", "/api/v1/accounts/"+acctA, "", memberCookie)
	mustStatus(t, rec, http.StatusOK, "member get granted")
	rec, _ = doReq(t, h, "POST", "/api/v1/accounts/"+acctA+"/qr", "", memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "viewer qr")

	// 4. Member cannot create accounts (needs accounts:pair).
	rec, _ = doReq(t, h, "POST", "/api/v1/accounts", `{"label":"Nope"}`, memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "member create")

	// 5. Campaign create: explicit ungranted account → 400; granted-viewer
	// drafts → 201 (view access drafts, only sending needs use).
	rec, _ = doReq(t, h, "POST", "/api/v1/campaigns",
		fmt.Sprintf(`{"name":"c1","bodyTemplate":"hi","wa_account_id":%q}`, acctB), memberCookie)
	mustStatus(t, rec, http.StatusBadRequest, "campaign ungranted account")
	rec, _ = doReq(t, h, "POST", "/api/v1/campaigns",
		fmt.Sprintf(`{"name":"c2","bodyTemplate":"hi","wa_account_id":%q}`, acctA), memberCookie)
	mustStatus(t, rec, http.StatusCreated, "campaign granted account")
	var camp struct {
		ID          string `json:"id"`
		WaAccountID string `json:"waAccountId"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&camp)
	if camp.WaAccountID != acctA {
		t.Fatalf("campaign account = %q", camp.WaAccountID)
	}
	// 5b. Viewer launches → 403 (send needs use); admin launches → 202.
	rec, _ = doReq(t, h, "POST", "/api/v1/campaigns/"+camp.ID+"/start", "", memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "viewer start")
	rec, _ = doReq(t, h, "POST", "/api/v1/campaigns/"+camp.ID+"/start", "", adminCookie)
	mustStatus(t, rec, http.StatusAccepted, "admin start")
	rec, _ = doReq(t, h, "POST", "/api/v1/campaigns/"+camp.ID+"/cancel", "", adminCookie)
	mustStatus(t, rec, http.StatusAccepted, "admin cancel")
	rec, _ = doReq(t, h, "DELETE", "/api/v1/campaigns/"+camp.ID, "", adminCookie)
	mustStatus(t, rec, http.StatusNoContent, "delete campaign")

	// 6. Admin deletes B (no grants/campaigns on it) → 204.
	rec, _ = doReq(t, h, "DELETE", "/api/v1/accounts/"+acctB, "", adminCookie)
	mustStatus(t, rec, http.StatusNoContent, "delete B")

	// 7. Legacy single-account route resolves the sole remaining account.
	rec, _ = doReq(t, h, "GET", "/api/v1/connection", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "legacy status resolves")
}
