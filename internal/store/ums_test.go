package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestCanMatrix covers the v1 permission map (no DB).
func TestCanMatrix(t *testing.T) {
	if !Can(RoleAdmin, PermBillingManage) || !Can(RoleAdmin, "anything:else") {
		t.Fatal("admin must allow all")
	}
	for _, p := range []string{PermContactsManage, PermTemplatesManage, PermCampaignsManage, PermCampaignsSend, PermAnalyticsView, PermAccountsView, PermAccountsUse} {
		if !Can(RoleUser, p) {
			t.Fatalf("user must allow %s", p)
		}
	}
	for _, p := range []string{PermOrgManage, PermMembersManage, PermAccountsPair, PermAccountsManage, PermBillingManage, PermKeysManage} {
		if Can(RoleUser, p) {
			t.Fatalf("user must deny %s", p)
		}
	}
	if Can("ghost", PermAnalyticsView) || !ValidRole(RoleAdmin) || ValidRole("ghost") {
		t.Fatal("role validation wrong")
	}
}

// umsHandles opens owner+app handles (env-gated).
func umsHandles(t *testing.T) (owner, app *DB) {
	t.Helper()
	var err error
	owner, err = OpenPostgres(pgOwnerDSN(t))
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	app, err = OpenPostgresNoMigrate(pgAppDSN(t))
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return owner, app
}

// TestUMSUserLifecycle covers users, orgs, memberships and last-admin guard.
func TestUMSUserLifecycle(t *testing.T) {
	owner, _ := umsHandles(t)

	org, err := owner.CreateOrg("UMS Lifecycle Co")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })
	if org.Slug == "" {
		t.Fatal("empty slug")
	}

	hash := "bcrypt-placeholder-hash"
	u1, err := owner.CreateUser("ums-admin@example.com", "Admin One", hash)
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u1.ID) })
	if _, err := owner.CreateUser("ums-admin@example.com", "Dup", hash); err == nil {
		t.Fatal("duplicate email allowed")
	}
	if _, err := owner.CreateUser("not-an-email", "X", hash); err == nil {
		t.Fatal("bad email allowed")
	}
	if _, err := owner.CreateUser("ums-nopw@example.com", "X", ""); err == nil {
		t.Fatal("empty hash allowed")
	}
	if _, err := owner.GetUserByEmail("ums-admin@example.com"); err != nil {
		t.Fatalf("get by email: %v", err)
	}
	if h, err := owner.UserPasswordHash(u1.ID); err != nil || h != hash {
		t.Fatalf("password hash roundtrip: %v", err)
	}

	m1, err := owner.AddMembership(u1.ID, org.ID, RoleAdmin)
	if err != nil || m1.Role != RoleAdmin {
		t.Fatalf("add membership: %v", err)
	}
	// Last-admin demote/remove refused.
	if _, err := owner.SetMemberRole(u1.ID, org.ID, RoleUser); err == nil {
		t.Fatal("last-admin demote allowed")
	}
	if err := owner.RemoveMembership(u1.ID, org.ID); err == nil {
		t.Fatal("last-admin remove allowed")
	}
	u2, err := owner.CreateUser("ums-member@example.com", "Member", hash)
	if err != nil {
		t.Fatalf("create user2: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u2.ID) })
	if _, err := owner.AddMembership(u2.ID, org.ID, RoleUser); err != nil {
		t.Fatalf("add member: %v", err)
	}
	u3, err := owner.CreateUser("ums-admin2@example.com", "Admin Two", hash)
	if err != nil {
		t.Fatalf("create user3: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u3.ID) })
	if _, err := owner.AddMembership(u3.ID, org.ID, RoleAdmin); err != nil {
		t.Fatalf("add admin2: %v", err)
	}
	// Two admins: demote + remove now legal.
	if _, err := owner.SetMemberRole(u1.ID, org.ID, RoleUser); err != nil {
		t.Fatalf("demote with backup admin: %v", err)
	}
	members, err := owner.MembersByOrg(org.ID)
	if err != nil || len(members) != 3 {
		t.Fatalf("members = %v, err=%v", members, err)
	}
	if err := owner.RemoveMembership(u1.ID, org.ID); err != nil {
		t.Fatalf("remove demoted: %v", err)
	}
	if _, err := owner.SetMemberRole(u3.ID, org.ID, RoleUser); err == nil {
		t.Fatal("last remaining admin demote allowed")
	}

	// Slug uniquification.
	org2, err := owner.CreateOrg("UMS Lifecycle Co")
	if err != nil {
		t.Fatalf("second org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org2.ID) })
	if org2.Slug == org.Slug {
		t.Fatal("slug collision")
	}
}

// TestUMSInviteResetVerify covers token flows end to end.
func TestUMSInviteResetVerify(t *testing.T) {
	owner, _ := umsHandles(t)

	org, err := owner.CreateOrg("UMS Token Co")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })
	u, err := owner.CreateUser("ums-token@example.com", "Token", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID) })
	if _, err := owner.AddMembership(u.ID, org.ID, RoleAdmin); err != nil {
		t.Fatalf("membership: %v", err)
	}

	// Invite.
	digest := "digest-" + uuid.NewString()
	inv, err := owner.CreateInvite(org.ID, "Invite Org", "ums-invited@example.com", RoleUser, digest, u.ID, time.Now().Add(time.Hour))
	if err != nil {
		t.Fatalf("create invite: %v", err)
	}
	got, err := owner.GetInviteByTokenHash(digest)
	if err != nil || got.ID != inv.ID || got.OrgName != "Invite Org" {
		t.Fatalf("get invite: %+v %v", got, err)
	}
	invitee, err := owner.CreateUser("ums-invited@example.com", "Invitee", "hash")
	if err != nil {
		t.Fatalf("create invitee: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, invitee.ID) })
	if err := owner.AcceptInvite(inv.ID); err != nil {
		t.Fatalf("accept: %v", err)
	}
	if _, err := owner.AddMembership(invitee.ID, org.ID, inv.Role); err != nil {
		t.Fatalf("join: %v", err)
	}
	if _, err := owner.GetInviteByTokenHash(digest); err == nil {
		t.Fatal("accepted invite still usable")
	}

	// Reset.
	rdigest := "rdigest-" + uuid.NewString()
	if err := owner.CreatePasswordReset(u.ID, rdigest, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create reset: %v", err)
	}
	uid, err := owner.ConsumePasswordReset(rdigest)
	if err != nil || uid != u.ID {
		t.Fatalf("consume reset: %v", err)
	}
	if _, err := owner.ConsumePasswordReset(rdigest); err == nil {
		t.Fatal("reset reuse allowed")
	}
	if err := owner.SetUserPassword(u.ID, "new-hash"); err != nil {
		t.Fatalf("set password: %v", err)
	}
	if h, _ := owner.UserPasswordHash(u.ID); h != "new-hash" {
		t.Fatal("password not updated")
	}

	// Verify.
	vdigest := "vdigest-" + uuid.NewString()
	if err := owner.CreateEmailVerification(u.ID, vdigest, time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("create verification: %v", err)
	}
	if _, err := owner.ConsumeEmailVerification(vdigest); err != nil {
		t.Fatalf("consume verification: %v", err)
	}
	gu, _ := owner.GetUser(u.ID)
	if gu.EmailVerifiedAt == "" {
		t.Fatal("not marked verified")
	}
	if _, err := owner.ConsumeEmailVerification(vdigest); err == nil {
		t.Fatal("verification reuse allowed")
	}
}

// TestUMSSessions covers issue/lookup/switch/revoke.
func TestUMSSessions(t *testing.T) {
	owner, _ := umsHandles(t)

	orgA, _ := owner.CreateOrg("UMS Sess A")
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, orgA.ID) })
	orgB, _ := owner.CreateOrg("UMS Sess B")
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, orgB.ID) })
	u, err := owner.CreateUser("ums-sess@example.com", "Sess", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID) })
	if _, err := owner.AddMembership(u.ID, orgA.ID, RoleAdmin); err != nil {
		t.Fatalf("membership A: %v", err)
	}
	if _, err := owner.AddMembership(u.ID, orgB.ID, RoleUser); err != nil {
		t.Fatalf("membership B: %v", err)
	}

	s, err := owner.CreateSession(u.ID, orgA.ID, "tok-hash-1", time.Now().Add(time.Hour))
	if err != nil || s.OrgID != orgA.ID {
		t.Fatalf("create session: %v", err)
	}
	if _, err := owner.GetSessionByTokenHash("tok-hash-1"); err != nil {
		t.Fatalf("get session: %v", err)
	}
	if err := owner.SetSessionOrg("tok-hash-1", orgB.ID); err != nil {
		t.Fatalf("switch org: %v", err)
	}
	switched, _ := owner.GetSessionByTokenHash("tok-hash-1")
	if switched.OrgID != orgB.ID {
		t.Fatal("switch not applied")
	}
	if err := owner.RevokeSession("tok-hash-1"); err != nil {
		t.Fatalf("revoke: %v", err)
	}
	if _, err := owner.GetSessionByTokenHash("tok-hash-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("revoked session still live")
	}

	if _, err := owner.CreateSession(u.ID, orgA.ID, "tok-hash-2", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("session2: %v", err)
	}
	if _, err := owner.CreateSession(u.ID, orgB.ID, "tok-hash-3", time.Now().Add(time.Hour)); err != nil {
		t.Fatalf("session3: %v", err)
	}
	if err := owner.RevokeUserOrgSessions(u.ID, orgA.ID); err != nil {
		t.Fatalf("revoke org sessions: %v", err)
	}
	if _, err := owner.GetSessionByTokenHash("tok-hash-2"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("org session not revoked")
	}
	if _, err := owner.GetSessionByTokenHash("tok-hash-3"); err != nil {
		t.Fatal("other-org session wrongly revoked")
	}
	if err := owner.RevokeUserSessions(u.ID); err != nil {
		t.Fatalf("revoke all: %v", err)
	}
	if _, err := owner.GetSessionByTokenHash("tok-hash-3"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("session survived revoke-all")
	}
}

// TestUMSGrants covers account-grant management.
func TestUMSGrants(t *testing.T) {
	owner, _ := umsHandles(t)

	org, err := owner.CreateOrg("UMS Grant Co")
	if err != nil {
		t.Fatalf("create org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })
	u, err := owner.CreateUser("ums-grant@example.com", "Grant", "hash")
	if err != nil {
		t.Fatalf("create user: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID) })
	if _, err := owner.AddMembership(u.ID, org.ID, RoleUser); err != nil {
		t.Fatalf("membership: %v", err)
	}

	g, err := owner.SetGrant(u.ID, "wa-acc-1", org.ID, "member", "admin-id")
	if err != nil || g.Role != "member" {
		t.Fatalf("set grant: %v", err)
	}
	if _, err := owner.SetGrant(u.ID, "wa-acc-1", org.ID, "superuser", "admin-id"); err == nil {
		t.Fatal("bad grant role allowed")
	}
	gs, err := owner.GrantsForUser(u.ID, org.ID)
	if err != nil || len(gs) != 1 {
		t.Fatalf("list grants: %v %v", gs, err)
	}
	if err := owner.RemoveGrant(u.ID, "wa-acc-1"); err != nil {
		t.Fatalf("remove grant: %v", err)
	}
	if _, err := owner.GetGrant(u.ID, "wa-acc-1"); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("grant still present")
	}
}

// TestUMSFailClosed proves unscoped app handles see nothing post-007.
func TestUMSFailClosed(t *testing.T) {
	owner, app := umsHandles(t)
	ctx := context.Background()

	var n int
	if err := app.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&n); err != nil || n != 0 {
		t.Fatalf("unscoped app sees contacts: n=%d err=%v", n, err)
	}
	if _, err := app.Exec(`INSERT INTO contacts (id, phone, name) VALUES ('x', '+10000000000', 'x')`); err == nil {
		t.Fatal("unscoped app insert allowed")
	} else {
		_, _ = owner.Exec(`DELETE FROM contacts WHERE id = 'x'`)
	}
	// Scoped access still works.
	if err := app.WithOrg(ctx, DefaultOrgID, func(odb *DB) error {
		_, err := odb.CreateContact("+15550137991", "Failclosed", nil)
		return err
	}); err != nil {
		t.Fatalf("scoped create: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM contacts WHERE phone = '+15550137991'`) })
}
