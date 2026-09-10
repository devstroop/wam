package server

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/devstroop/wam/internal/store"
)

// hardeningHarness boots owner/app handles + harness mux.
func hardeningHarness(t *testing.T) (http.Handler, *store.DB) {
	t.Helper()
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
	return h, owner
}

func hxGet(t *testing.T, h http.Handler, path, cookie string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest("GET", path, nil)
	req.Header.Set("HX-Request", "true")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	return rec
}

// TestDialogZeroAccounts renders the unpaired notice (200, no console 400s).
func TestDialogZeroAccounts(t *testing.T) {
	h, owner := hardeningHarness(t)
	email := "http-dlg-admin@example.com"
	opsCleanup(t, owner, email)
	cookie, _ := opsSignup(t, h, owner, email)

	rec := hxGet(t, h, "/partials/connect-dialog", cookie)
	mustStatus(t, rec, http.StatusOK, "dialog zero accounts")
	if !strings.Contains(rec.Body.String(), "No numbers yet") {
		t.Fatalf("missing unpaired notice: %s", rec.Body.String())
	}
}

// TestDialogPicker lists accounts when ambiguous.
func TestDialogPicker(t *testing.T) {
	h, owner := hardeningHarness(t)
	email := "http-pick-admin@example.com"
	opsCleanup(t, owner, email)
	cookie, _ := opsSignup(t, h, owner, email)

	for _, label := range []string{"Sales", "Support"} {
		rec, _ := doReq(t, h, "POST", "/api/v1/accounts",
			fmt.Sprintf(`{"label":%q}`, label), cookie)
		if label == "Sales" {
			mustStatus(t, rec, http.StatusCreated, "create "+label)
			continue
		}
		// Free tier allows one number; second row bypasses quota (owner).
		mustStatus(t, rec, http.StatusPaymentRequired, "quota on second")
		user, _ := owner.GetUserByEmail(email)
		ms, _ := owner.MembershipsByUser(user.ID)
		if _, err := owner.CreateAccount(ms[0].OrgID, label, user.ID); err != nil {
			t.Fatalf("owner second account: %v", err)
		}
	}
	rec := hxGet(t, h, "/partials/connect-dialog", cookie)
	mustStatus(t, rec, http.StatusOK, "dialog picker")
	body := rec.Body.String()
	if !strings.Contains(body, "Choose a number") || !strings.Contains(body, "Sales") || !strings.Contains(body, "Support") {
		t.Fatalf("missing picker: %s", body)
	}
	rec = hxGet(t, h, "/partials/account-menu", cookie)
	mustStatus(t, rec, http.StatusOK, "account menu")
	mbody := rec.Body.String()
	if !strings.Contains(mbody, email) || !strings.Contains(mbody, "Sign Out") {
		t.Fatalf("menu missing identity: %s", mbody)
	}
}

// TestAccountMenuSwitch exercises org switching via form post.
func TestAccountMenuSwitch(t *testing.T) {
	h, owner := hardeningHarness(t)
	email := "http-switch-admin@example.com"
	opsCleanup(t, owner, email)
	cookie, orgA := opsSignup(t, h, owner, email)

	user, _ := owner.GetUserByEmail(email)
	orgB, err := owner.CreateOrg("Second Workspace")
	if err != nil {
		t.Fatalf("org B: %v", err)
	}
	if _, err := owner.AddMembership(user.ID, orgB.ID, store.RoleAdmin); err != nil {
		t.Fatalf("join B: %v", err)
	}

	// Menu shows the switcher with both orgs.
	rec := hxGet(t, h, "/partials/account-menu", cookie)
	mustStatus(t, rec, http.StatusOK, "menu")
	if !strings.Contains(rec.Body.String(), "Second Workspace") {
		t.Fatalf("missing second org: %s", rec.Body.String())
	}
	// Form switch → HX-Refresh, session moved.
	req := httptest.NewRequest("POST", "/api/v1/auth/switch", strings.NewReader("org_id="+orgB.ID))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Cookie", cookie)
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	mustStatus(t, rec2, http.StatusOK, "switch")
	if rec2.Header().Get("HX-Refresh") == "" {
		t.Fatal("missing HX-Refresh on switch")
	}
	rec, _ = doReq(t, h, "GET", "/api/v1/auth/me", "", cookie)
	var me struct {
		OrgID string `json:"orgId"`
	}
	if err := json.NewDecoder(rec.Body).Decode(&me); err != nil {
		t.Fatalf("decode me: %v", err)
	}
	if me.OrgID != orgB.ID {
		t.Fatalf("org = %s, want %s (from %s)", me.OrgID, orgB.ID, orgA)
	}
}

// TestKeysFormFlow covers form mint (one-time token partial) + revoke.
func TestKeysFormFlow(t *testing.T) {
	h, owner := hardeningHarness(t)
	email := "http-keyform-admin@example.com"
	opsCleanup(t, owner, email)
	cookie, _ := opsSignup(t, h, owner, email)

	req := httptest.NewRequest("POST", "/api/v1/keys",
		strings.NewReader("name=ci&scopes=contacts%3Amanage&scopes=analytics%3Aview"))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	req.Header.Set("HX-Request", "true")
	req.Header.Set("Cookie", cookie)
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, req)
	mustStatus(t, rec, http.StatusOK, "mint form")
	if !strings.Contains(rec.Body.String(), "wam_") || !strings.Contains(rec.Body.String(), "shown once") {
		t.Fatalf("missing one-time token: %s", rec.Body.String())
	}
	if rec.Header().Get("HX-Trigger") == "" {
		t.Fatal("missing toast trigger")
	}
}

// TestWebhooksFormFlow covers form create + validation + delete.
func TestWebhooksFormFlow(t *testing.T) {
	h, owner := hardeningHarness(t)
	email := "http-whform-admin@example.com"
	opsCleanup(t, owner, email)
	cookie, _ := opsSignup(t, h, owner, email)

	post := func(body string) *httptest.ResponseRecorder {
		req := httptest.NewRequest("POST", "/api/v1/webhooks", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		req.Header.Set("HX-Request", "true")
		req.Header.Set("Cookie", cookie)
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, req)
		return rec
	}
	rec := post("url=http%3A%2F%2Fx.example.com%2Fh&events=message.sent")
	mustStatus(t, rec, http.StatusBadRequest, "http rejected")
	rec = post("url=https%3A%2F%2Fx.example.com%2Fh&secret=s&events=message.sent&events=campaign.done")
	mustStatus(t, rec, http.StatusCreated, "create form")
	if rec.Header().Get("HX-Refresh") == "" {
		t.Fatal("missing HX-Refresh")
	}
}
