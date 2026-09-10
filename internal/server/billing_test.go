package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"testing"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/store"
)

// TestBillingHTTPQuota drives plan assignment + 402 enforcement over HTTP.
func TestBillingHTTPQuota(t *testing.T) {
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

	adminEmail := "http-bill-admin@example.com"
	memberEmail := "http-bill-member@example.com"
	t.Cleanup(func() {
		for _, email := range []string{adminEmail, memberEmail} {
			u, err := owner.GetUserByEmail(email)
			if err != nil {
				continue
			}
			ms, _ := owner.MembershipsByUser(u.ID)
			_, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID)
			for _, m := range ms {
				// contacts/campaigns carry org_id without FK cascade: clear
				// tenant rows before dropping an emptied org.
				_, _ = owner.Exec(`DELETE FROM campaign_recipients WHERE campaign_id IN (SELECT id FROM campaigns WHERE org_id = ?)`, m.OrgID)
				_, _ = owner.Exec(`DELETE FROM campaigns WHERE org_id = ?`, m.OrgID)
				_, _ = owner.Exec(`DELETE FROM contacts WHERE org_id = ?`, m.OrgID)
				_, _ = owner.Exec(`DELETE FROM wa_accounts WHERE org_id = ?`, m.OrgID)
				if n, _ := owner.MembersByOrg(m.OrgID); len(n) == 0 {
					_, _ = owner.Exec(`DELETE FROM orgs WHERE id = ? AND id != 'org_default'`, m.OrgID)
				}
			}
			_, _ = owner.Exec(`DELETE FROM users WHERE email = ?`, email)
		}
		if _, err := owner.Exec(`DELETE FROM plans WHERE id = 'plan_http_tiny'`); err != nil {
			t.Logf("cleanup plan: %v", err)
		}
	})

	// Admin via real signup.
	rec, _ := doReq(t, h, "POST", "/signup",
		fmt.Sprintf(`{"email":%q,"name":"Bill","password":"password123"}`, adminEmail), "")
	mustStatus(t, rec, http.StatusCreated, "signup")
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	_ = owner.VerifyUserEmail(adminUser.ID)
	rec, adminCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, adminEmail), "")
	mustStatus(t, rec, http.StatusOK, "login")

	// Member joins (store-level), logs in.
	memberHash, err := auth.HashPassword("password123")
	if err != nil {
		t.Fatalf("hash: %v", err)
	}
	memberUser, err := owner.CreateUser(memberEmail, "Member", memberHash)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	orgID := adminMs[0].OrgID
	if _, err := owner.AddMembership(memberUser.ID, orgID, store.RoleUser); err != nil {
		t.Fatalf("join: %v", err)
	}
	_ = owner.VerifyUserEmail(memberUser.ID)
	rec, memberCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member login")

	// 1. Plan overview defaults to free with zero usage.
	rec, _ = doReq(t, h, "GET", "/api/v1/billing/plan", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "plan overview")
	var overview struct {
		Plan struct {
			Code string `json:"code"`
		} `json:"plan"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&overview)
	if overview.Plan.Code != "free" {
		t.Fatalf("plan = %s", overview.Plan.Code)
	}

	// 2. Member cannot switch plans (needs billing:manage).
	rec, _ = doReq(t, h, "POST", "/api/v1/billing/subscription", `{"plan_id":"starter"}`, memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "member switch plan")

	// 3. Tiny plan (1 contact) via owner; admin assigns via API.
	if _, err := owner.Exec(`INSERT INTO plans (id, code, name, limits, price_paise)
		VALUES ('plan_http_tiny', 'http_tiny', 'HTTP Tiny', '{"accounts":1,"contacts":1,"msgs_per_month":1000,"members":5}', 0)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	rec, _ = doReq(t, h, "POST", "/api/v1/billing/subscription", `{"plan_id":"http_tiny"}`, adminCookie)
	mustStatus(t, rec, http.StatusOK, "assign tiny")

	// 4. First contact ok, second → 402.
	rec, _ = doReq(t, h, "POST", "/api/v1/contacts", `{"phone":"+15550169101","name":"One"}`, adminCookie)
	mustStatus(t, rec, http.StatusCreated, "contact 1")
	rec, _ = doReq(t, h, "POST", "/api/v1/contacts", `{"phone":"+15550169102","name":"Two"}`, adminCookie)
	mustStatus(t, rec, http.StatusPaymentRequired, "contact 2 over quota")
}
