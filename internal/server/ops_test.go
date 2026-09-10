package server

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/tls"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/notify"
	"github.com/devstroop/wam/internal/store"
)

// opsSignup creates a verified admin via HTTP, returning cookie + user.
func opsSignup(t *testing.T, h http.Handler, owner *store.DB, email string) (string, string) {
	t.Helper()
	rec, _ := doReq(t, h, "POST", "/signup",
		fmt.Sprintf(`{"email":%q,"name":"Ops","password":"password123"}`, email), "")
	mustStatus(t, rec, http.StatusCreated, "signup "+email)
	u, err := owner.GetUserByEmail(email)
	if err != nil {
		t.Fatalf("user %s: %v", email, err)
	}
	_ = owner.VerifyUserEmail(u.ID)
	rec, cookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, email), "")
	mustStatus(t, rec, http.StatusOK, "login "+email)
	ms, _ := owner.MembershipsByUser(u.ID)
	return cookie, ms[0].OrgID
}

func opsCleanup(t *testing.T, owner *store.DB, emails ...string) {
	t.Helper()
	t.Cleanup(func() {
		for _, email := range emails {
			u, err := owner.GetUserByEmail(email)
			if err != nil {
				continue
			}
			ms, _ := owner.MembershipsByUser(u.ID)
			_, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID)
			for _, m := range ms {
				_, _ = owner.Exec(`DELETE FROM api_keys WHERE org_id = ?`, m.OrgID)
				_, _ = owner.Exec(`DELETE FROM webhook_endpoints WHERE org_id = ?`, m.OrgID)
				_, _ = owner.Exec(`DELETE FROM webhook_deliveries WHERE org_id = ?`, m.OrgID)
				_, _ = owner.Exec(`DELETE FROM audit_logs WHERE org_id = ?`, m.OrgID)
				if n, _ := owner.MembersByOrg(m.OrgID); len(n) == 0 {
					_, _ = owner.Exec(`DELETE FROM orgs WHERE id = ? AND id != 'org_default'`, m.OrgID)
				}
			}
			_, _ = owner.Exec(`DELETE FROM users WHERE email = ?`, email)
		}
	})
}

// TestOpsKeysAndBearer covers mint → Bearer use → scope deny → revoke.
func TestOpsKeysAndBearer(t *testing.T) {
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

	adminEmail := "http-ops-admin@example.com"
	memberEmail := "http-ops-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	// Member joins admin's org (store-level).
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	hash, _ := auth.HashPassword("password123")
	memberUser, _ := owner.CreateUser(memberEmail, "Member", hash)
	_, _ = owner.AddMembership(memberUser.ID, adminMs[0].OrgID, store.RoleUser)
	_ = owner.VerifyUserEmail(memberUser.ID)
	rec, memberCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member login")

	bearer := func(token string) string { return "Bearer " + token }

	// 1. Member cannot mint (needs keys:manage).
	rec, _ = doReq(t, h, "POST", "/api/v1/keys", `{"name":"x","scopes":["contacts:manage"]}`, memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "member mint")

	// 2. Admin mints a narrow key.
	rec, _ = doReq(t, h, "POST", "/api/v1/keys", `{"name":"ci","scopes":["contacts:manage"]}`, adminCookie)
	mustStatus(t, rec, http.StatusCreated, "mint")
	var minted struct {
		Token string `json:"token"`
		Key   struct {
			ID string `json:"id"`
		} `json:"key"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&minted)
	if minted.Token == "" || minted.Key.ID == "" {
		t.Fatal("no raw token returned")
	}

	// 3. Bearer reads contacts (member-level read allowed).
	req := httptest.NewRequest("GET", "/api/v1/contacts", nil)
	req.Header.Set("Authorization", bearer(minted.Token))
	rec2 := httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	mustStatus(t, rec2, http.StatusOK, "bearer read")

	// 4. Bearer outside its scopes → 403 (members needs members:manage).
	req = httptest.NewRequest("GET", "/api/v1/members", nil)
	req.Header.Set("Authorization", bearer(minted.Token))
	rec2 = httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	mustStatus(t, rec2, http.StatusForbidden, "bearer scope deny")

	// 5. Bad token → 401 (no cookie fallback).
	req = httptest.NewRequest("GET", "/api/v1/contacts", nil)
	req.Header.Set("Authorization", bearer(minted.Token+"tampered"))
	rec2 = httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	mustStatus(t, rec2, http.StatusUnauthorized, "bad bearer")

	// 6. Revoke → 401.
	rec, _ = doReq(t, h, "DELETE", "/api/v1/keys/"+minted.Key.ID, "", adminCookie)
	mustStatus(t, rec, http.StatusNoContent, "revoke")
	req = httptest.NewRequest("GET", "/api/v1/contacts", nil)
	req.Header.Set("Authorization", bearer(minted.Token))
	rec2 = httptest.NewRecorder()
	h.ServeHTTP(rec2, req)
	mustStatus(t, rec2, http.StatusUnauthorized, "revoked bearer")

	// 7. Audit trail captured login + key lifecycle.
	rec, _ = doReq(t, h, "GET", "/api/v1/audit?limit=50", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "audit list")
	var trail struct {
		Data []map[string]any `json:"data"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&trail)
	seen := map[string]bool{}
	for _, e := range trail.Data {
		if a, ok := e["action"].(string); ok {
			seen[a] = true
		}
	}
	for _, want := range []string{"auth.login", "keys.create", "keys.revoke"} {
		if !seen[want] {
			t.Fatalf("audit missing %s (have %v)", want, seen)
		}
	}
	// Member cannot read audit.
	rec, _ = doReq(t, h, "GET", "/api/v1/audit", "", memberCookie)
	mustStatus(t, rec, http.StatusForbidden, "member audit")
}

// TestOpsWebhooksAndMetrics covers endpoint CRUD, signed delivery, metrics.
func TestOpsWebhooksAndMetrics(t *testing.T) {
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

	adminEmail := "http-ops-wh@example.com"
	opsCleanup(t, owner, adminEmail)
	adminCookie, orgID := opsSignup(t, h, owner, adminEmail)

	// 1. Validation: http + unknown events refused.
	rec, _ := doReq(t, h, "POST", "/api/v1/webhooks",
		`{"url":"http://x.example.com/h","events":["message.sent"]}`, adminCookie)
	mustStatus(t, rec, http.StatusBadRequest, "http url")
	rec, _ = doReq(t, h, "POST", "/api/v1/webhooks",
		`{"url":"https://x.example.com/h","events":["nope"]}`, adminCookie)
	mustStatus(t, rec, http.StatusBadRequest, "bad event")

	// 2. Sink server captures deliveries + signatures.
	var got int32
	var gotSig, gotEvent string
	var gotBody []byte
	sink := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&got, 1)
		gotSig = r.Header.Get("X-Wam-Signature")
		gotEvent = r.Header.Get("X-Wam-Event")
		buf := make([]byte, 1<<20)
		n, _ := r.Body.Read(buf)
		gotBody = buf[:n]
		w.WriteHeader(http.StatusOK)
	}))
	defer sink.Close()

	rec, _ = doReq(t, h, "POST", "/api/v1/webhooks",
		fmt.Sprintf(`{"url":%q,"secret":"whsec","events":["message.sent"]}`, sink.URL), adminCookie)
	mustStatus(t, rec, http.StatusCreated, "create endpoint")
	var created struct {
		ID string `json:"id"`
	}
	_ = json.NewDecoder(rec.Body).Decode(&created)

	// 3. Emit bypasses HTTP: dispatcher directly against the pool handle
	// (insecure TLS only for the httptest sink).
	disp := &notify.Dispatcher{Store: app, HTTP: &http.Client{
		Transport: &http.Transport{TLSClientConfig: &tls.Config{InsecureSkipVerify: true}},
	}}
	disp.Emit(orgID, "message.sent", map[string]any{"phone": "+10000000001"})
	deadline(t, func() bool { return atomic.LoadInt32(&got) >= 1 }, "delivery")
	if gotEvent != "message.sent" {
		t.Fatalf("event header = %q", gotEvent)
	}
	mac := hmac.New(sha256.New, []byte("whsec"))
	mac.Write(gotBody)
	if hex.EncodeToString(mac.Sum(nil)) != gotSig {
		t.Fatal("delivery signature mismatch")
	}

	// 4. List shows endpoint (no secret); delete removes.
	rec, _ = doReq(t, h, "GET", "/api/v1/webhooks", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "list endpoints")
	if strings.Contains(rec.Body.String(), "whsec") {
		t.Fatal("secret leaked in list")
	}
	rec, _ = doReq(t, h, "DELETE", "/api/v1/webhooks/"+created.ID, "", adminCookie)
	mustStatus(t, rec, http.StatusNoContent, "delete endpoint")

	// 5. Metrics scrape (cookie auth) exposes gauges.
	rec, _ = doReq(t, h, "GET", "/metrics", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "metrics")
	for _, want := range []string{"wam_contacts", "wam_accounts_total", "wam_uptime_seconds"} {
		if !strings.Contains(rec.Body.String(), want) {
			t.Fatalf("metrics missing %s", want)
		}
	}
	_ = orgID
}

func deadline(t *testing.T, cond func() bool, what string) {
	t.Helper()
	for i := 0; i < 100; i++ {
		if cond() {
			return
		}
		time.Sleep(50 * time.Millisecond)
	}
	t.Fatalf("timeout waiting for %s", what)
}
