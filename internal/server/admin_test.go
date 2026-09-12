package server

import (
	"fmt"
	"net/http"
	"strings"
	"testing"

	"github.com/devstroop/wam/internal/auth"
	"github.com/devstroop/wam/internal/store"
)

// TestAdminOverview covers the admin layout end to end: admin renders with
// org stats + activity, members get 403, logged-out users redirect.
func TestAdminOverview(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-overview@example.com"
	memberEmail := "http-admin-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)

	hash, _ := auth.HashPassword("password123")
	memberUser, err := owner.CreateUser(memberEmail, "Member", hash)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	if _, err := owner.AddMembership(memberUser.ID, adminMs[0].OrgID, store.RoleUser); err != nil {
		t.Fatalf("join: %v", err)
	}
	_ = owner.VerifyUserEmail(memberUser.ID)
	rec, memberCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member login")

	// Admin renders inside the admin layout.
	rec, _ = doReq(t, h, "GET", "/admin", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin overview")
	body := rec.Body.String()
	for _, want := range []string{
		"Back to Dashboard",       // admin layout sidebar
		"Recent activity",         // overview section
		"Plan &amp; usage",        // billing card
		"aria-current=\"page\"",   // active nav marker
		`data-nav-group="access"`, // expandable access group present…
		`data-open="false"`,       // …collapsed off-group on overview
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin page missing %q", want)
		}
	}
	if strings.Contains(body, "sidebar-text\">Dashboard<") {
		t.Fatal("admin layout leaked app sidebar")
	}

	// Member is forbidden (page, so redirect per gate convention).
	rec, _ = doReq(t, h, "GET", "/admin", "", memberCookie)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusForbidden {
		t.Fatalf("member /admin = %d, want redirect or 403", rec.Code)
	}

	// Admin menu links the admin area; member menu must not.
	mrec := hxGet(t, h, "/partials/account-menu", memberCookie)
	mustStatus(t, mrec, http.StatusOK, "member menu")
	if strings.Contains(mrec.Body.String(), `href="/admin"`) {
		t.Fatal("member menu leaks admin entry")
	}
	arec := hxGet(t, h, "/partials/account-menu", adminCookie)
	mustStatus(t, arec, http.StatusOK, "admin menu")
	if !strings.Contains(arec.Body.String(), `href="/admin"`) {
		t.Fatal("admin menu missing admin entry")
	}

	// Logged out redirects to login.
	rec, _ = doReq(t, h, "GET", "/admin", "", "")
	mustStatus(t, rec, http.StatusSeeOther, "anon redirect")
}

// TestAdminUsersPage covers the full user-management page: admin sees
// members, invite and grant controls; members get redirect/403.
func TestAdminUsersPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-users@example.com"
	memberEmail := "http-admin-users-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)

	hash, _ := auth.HashPassword("password123")
	memberUser, err := owner.CreateUser(memberEmail, "Member", hash)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	if _, err := owner.AddMembership(memberUser.ID, adminMs[0].OrgID, store.RoleUser); err != nil {
		t.Fatalf("join: %v", err)
	}
	_ = owner.VerifyUserEmail(memberUser.ID)
	rec, memberCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member login")

	rec, _ = doReq(t, h, "GET", "/admin/users", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin users page")
	body := rec.Body.String()
	for _, want := range []string{
		"Users",                   // page head
		"Invite member",           // invite form
		"Grant number access",     // grant form
		memberEmail,               // member row
		"Back to Dashboard",       // admin layout sidebar
		`>Access<`,                // access group label
		`data-nav-group="access"`, // expandable group
		`data-open="true"`,        // auto-expanded on group page
		`href="/admin/roles"`,     // admin layout nav
		`href="/admin/grants"`,    // grants page (was an anchor into users)
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("users page missing %q", want)
		}
	}

	rec, _ = doReq(t, h, "GET", "/admin/users", "", memberCookie)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusForbidden {
		t.Fatalf("member /admin/users = %d, want redirect or 403", rec.Code)
	}
}

// TestAdminRolesPage covers the permission matrix: all permissions listed,
// admin column full, user column partial.
func TestAdminRolesPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-roles@example.com"
	opsCleanup(t, owner, adminEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)

	rec, _ := doReq(t, h, "GET", "/admin/roles", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin roles page")
	body := rec.Body.String()
	for _, want := range []string{
		"Permission matrix",
		"members:manage",
		"campaigns:send",
		"keys:manage",
		`href="/admin/users"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("roles page missing %q", want)
		}
	}
	// 13 permissions × (header + rows): every permission renders a row.
	if got := strings.Count(body, "<code>"); got < len(store.AllPermissions()) {
		t.Fatalf("matrix rows = %d, want >= %d", got, len(store.AllPermissions()))
	}
}

// TestAdminGrantsPage covers the dedicated grants page: admin sees the
// grant form, a created grant row with revoke, and the expanded Access
// group with Grants marked current; members get redirect/403.
func TestAdminGrantsPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-grants@example.com"
	memberEmail := "http-admin-grants-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)

	hash, _ := auth.HashPassword("password123")
	memberUser, err := owner.CreateUser(memberEmail, "Member", hash)
	if err != nil {
		t.Fatalf("member: %v", err)
	}
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	orgID := adminMs[0].OrgID
	if _, err := owner.AddMembership(memberUser.ID, orgID, store.RoleUser); err != nil {
		t.Fatalf("join: %v", err)
	}
	_ = owner.VerifyUserEmail(memberUser.ID)
	acc, err := owner.CreateAccount(orgID, "Grants Page Line", adminUser.ID)
	if err != nil {
		t.Fatalf("account: %v", err)
	}
	if _, err := owner.SetGrant(memberUser.ID, acc.ID, orgID, "viewer", adminUser.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}
	rec, memberCookie := doReq(t, h, "POST", "/login",
		fmt.Sprintf(`{"email":%q,"password":"password123"}`, memberEmail), "")
	mustStatus(t, rec, http.StatusOK, "member login")

	rec, _ = doReq(t, h, "GET", "/admin/grants", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin grants page")
	body := rec.Body.String()
	for _, want := range []string{
		"Grants",               // page head
		"Grant number access",  // grant form
		"Current grants",       // grants table
		memberEmail,            // grant row member
		"Grants Page Line",     // grant row number
		"viewer",               // grant role
		"Revoke",               // revoke control
		`>Access<`,             // access group label
		`data-open="true"`,     // auto-expanded on group page
		`href="/admin/grants"`, // grants nav item
		`aria-current="page"`,  // grants marked current
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("grants page missing %q", want)
		}
	}
	if strings.Contains(body, "No grants yet") {
		t.Fatal("grants page shows empty state despite a grant")
	}

	rec, _ = doReq(t, h, "GET", "/admin/grants", "", memberCookie)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusForbidden {
		t.Fatalf("member /admin/grants = %d, want redirect or 403", rec.Code)
	}

	rec, _ = doReq(t, h, "GET", "/admin/grants", "", "")
	mustStatus(t, rec, http.StatusSeeOther, "anon redirect")
}

// adminMemberCookie creates a member in the admin's org and returns a login
// cookie for gate tests (member must not reach admin-only pages).
func adminMemberCookie(t *testing.T, h http.Handler, owner *store.DB, orgID, memberEmail string) string {
	t.Helper()
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
	return memberCookie
}

// adminGateAsserts covers the common gate: member redirect/403, anon redirect.
func adminGateAsserts(t *testing.T, h http.Handler, path, memberCookie string) {
	t.Helper()
	rec, _ := doReq(t, h, "GET", path, "", memberCookie)
	if rec.Code != http.StatusSeeOther && rec.Code != http.StatusForbidden {
		t.Fatalf("member %s = %d, want redirect or 403", path, rec.Code)
	}
	rec, _ = doReq(t, h, "GET", path, "", "")
	mustStatus(t, rec, http.StatusSeeOther, "anon redirect")
}

// TestAdminKeysPage covers the dedicated keys page: form + empty state for
// a fresh org, nav marked current, gates hold.
func TestAdminKeysPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-keys@example.com"
	memberEmail := "http-admin-keys-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	memberCookie := adminMemberCookie(t, h, owner, adminMs[0].OrgID, memberEmail)

	rec, _ := doReq(t, h, "GET", "/admin/keys", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin keys page")
	body := rec.Body.String()
	for _, want := range []string{
		"API keys",            // page head
		"Create key",          // mint form
		"No keys yet.",        // empty state
		`href="/admin/keys"`,  // nav item
		`aria-current="page"`, // keys marked current
		"Back to Dashboard",   // admin layout sidebar
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("keys page missing %q", want)
		}
	}
	adminGateAsserts(t, h, "/admin/keys", memberCookie)
}

// TestAdminWebhooksPage covers the dedicated webhooks page.
func TestAdminWebhooksPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-webhooks@example.com"
	memberEmail := "http-admin-webhooks-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	memberCookie := adminMemberCookie(t, h, owner, adminMs[0].OrgID, memberEmail)

	rec, _ := doReq(t, h, "GET", "/admin/webhooks", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin webhooks page")
	body := rec.Body.String()
	for _, want := range []string{
		"Webhooks",               // page head
		"Add endpoint",           // create form
		"No endpoints yet.",      // empty state
		`href="/admin/webhooks"`, // nav item
		`aria-current="page"`,    // webhooks marked current
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("webhooks page missing %q", want)
		}
	}
	adminGateAsserts(t, h, "/admin/webhooks", memberCookie)
}

// TestAdminBillingPage covers the dedicated plan & usage page.
func TestAdminBillingPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-billing@example.com"
	memberEmail := "http-admin-billing-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	memberCookie := adminMemberCookie(t, h, owner, adminMs[0].OrgID, memberEmail)

	rec, _ := doReq(t, h, "GET", "/admin/billing", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin billing page")
	body := rec.Body.String()
	for _, want := range []string{
		"Plan &amp; usage",      // page head
		"Messages this month",   // usage row (summary resolves on fresh org)
		`href="/admin/billing"`, // nav item
		`aria-current="page"`,   // billing marked current
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("billing page missing %q", want)
		}
	}
	adminGateAsserts(t, h, "/admin/billing", memberCookie)
}

// TestAccountsPage covers the user numbers page: admin sees all numbers +
// lifecycle controls; a granted member sees only their number, read-only.
func TestAccountsPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-accounts@example.com"
	memberEmail := "http-accounts-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	orgID := adminMs[0].OrgID
	memberCookie := adminMemberCookie(t, h, owner, orgID, memberEmail)
	memberUser, _ := owner.GetUserByEmail(memberEmail)

	accA, err := owner.CreateAccount(orgID, "Sales line", adminUser.ID)
	if err != nil {
		t.Fatalf("account A: %v", err)
	}
	if _, err := owner.CreateAccount(orgID, "Support line", adminUser.ID); err != nil {
		t.Fatalf("account B: %v", err)
	}
	if _, err := owner.SetGrant(memberUser.ID, accA.ID, orgID, "viewer", adminUser.ID); err != nil {
		t.Fatalf("grant: %v", err)
	}

	rec, _ := doReq(t, h, "GET", "/accounts", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin accounts page")
	body := rec.Body.String()
	for _, want := range []string{
		"Numbers",                    // page head
		"Sales line", "Support line", // all numbers
		"Add number", // lifecycle control
		"Pair / manage",
		`href="/accounts"`,    // nav item
		`aria-current="page"`, // numbers marked current
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("accounts page missing %q", want)
		}
	}

	// Member: granted number only, no lifecycle controls.
	rec, _ = doReq(t, h, "GET", "/accounts", "", memberCookie)
	mustStatus(t, rec, http.StatusOK, "member accounts page")
	mbody := rec.Body.String()
	for _, want := range []string{"Sales line", `href="/accounts"`} {
		if !strings.Contains(mbody, want) {
			t.Fatalf("member accounts page missing %q", want)
		}
	}
	for _, want := range []string{"Support line", "Add number", "Pair / manage", "Delete"} {
		if strings.Contains(mbody, want) {
			t.Fatalf("member accounts page leaks %q", want)
		}
	}

	rec, _ = doReq(t, h, "GET", "/accounts", "", "")
	mustStatus(t, rec, http.StatusSeeOther, "anon redirect")
}

// TestAdminAccountsPage covers the admin numbers page: all numbers +
// controls for admins; members get redirect/403.
func TestAdminAccountsPage(t *testing.T) {
	h, owner := hardeningHarness(t)
	adminEmail := "http-admin-accounts@example.com"
	memberEmail := "http-admin-accounts-member@example.com"
	opsCleanup(t, owner, adminEmail, memberEmail)

	adminCookie, _ := opsSignup(t, h, owner, adminEmail)
	adminUser, _ := owner.GetUserByEmail(adminEmail)
	adminMs, _ := owner.MembershipsByUser(adminUser.ID)
	orgID := adminMs[0].OrgID
	memberCookie := adminMemberCookie(t, h, owner, orgID, memberEmail)

	if _, err := owner.CreateAccount(orgID, "Admin line", adminUser.ID); err != nil {
		t.Fatalf("account: %v", err)
	}

	rec, _ := doReq(t, h, "GET", "/admin/accounts", "", adminCookie)
	mustStatus(t, rec, http.StatusOK, "admin accounts page")
	body := rec.Body.String()
	for _, want := range []string{
		"Numbers",                // page head
		"Admin line",             // number row
		"Add number",             // create form
		"Pair / manage",          // dialog entry
		`href="/admin/accounts"`, // nav item
		`aria-current="page"`,    // numbers marked current
		"Back to Dashboard",      // admin layout sidebar
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("admin accounts page missing %q", want)
		}
	}
	adminGateAsserts(t, h, "/admin/accounts", memberCookie)
}
