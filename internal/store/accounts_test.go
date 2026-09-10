package store

import (
	"context"
	"database/sql"
	"errors"
	"testing"
)

// TestCanAccountMatrix covers grant checks without a DB.
func TestCanAccountMatrix(t *testing.T) {
	if !CanAccount(RoleAdmin, nil, "a", true) {
		t.Fatal("admin denied")
	}
	if !CanAccount("", nil, "a", true) {
		t.Fatal("legacy denied")
	}
	grants := map[string]string{"a": "member", "b": "viewer"}
	if !CanAccount(RoleUser, grants, "a", true) || !CanAccount(RoleUser, grants, "a", false) {
		t.Fatal("member grant wrong")
	}
	if CanAccount(RoleUser, grants, "b", true) || !CanAccount(RoleUser, grants, "b", false) {
		t.Fatal("viewer grant wrong")
	}
	if CanAccount(RoleUser, grants, "c", false) || CanAccount("ghost", grants, "a", false) {
		t.Fatal("missing/unknown grant allowed")
	}
	if ResolveDefaultAccount(nil) != "" || ResolveDefaultAccount([]Account{{ID: "a"}, {ID: "b"}}) != "" {
		t.Fatal("default resolution wrong for 0/2")
	}
	if ResolveDefaultAccount([]Account{{ID: "only"}}) != "only" {
		t.Fatal("default resolution wrong for 1")
	}
}

func accountTestOrgs(t *testing.T, owner *DB) (*Org, *Org) {
	t.Helper()
	oa, err := owner.CreateOrg("Multi Acct Org A")
	if err != nil {
		t.Fatalf("org A: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, oa.ID) })
	ob, err := owner.CreateOrg("Multi Acct Org B")
	if err != nil {
		t.Fatalf("org B: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, ob.ID) })
	return oa, ob
}

// TestAccountsCRUD covers the account lifecycle + RLS isolation.
func TestAccountsCRUD(t *testing.T) {
	owner, app := umsHandles(t)
	oa, ob := accountTestOrgs(t, owner)
	ctx := context.Background()

	a, err := owner.CreateAccount(oa.ID, "Sales Line", "admin-1")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if a.Status != AccountPending || a.OrgID != oa.ID {
		t.Fatalf("account = %+v", a)
	}
	if _, err := owner.CreateAccount("no-such-org", "X", ""); err == nil {
		t.Fatal("account in missing org allowed")
	}
	if err := owner.MarkAccountPaired(a.ID, "+15550130001", "15550130001@s.whatsapp.net", "15550130001@s.whatsapp.net"); err != nil {
		t.Fatalf("paired: %v", err)
	}
	got, err := owner.GetAccount(a.ID)
	if err != nil || got.Status != AccountConnected || got.Phone != "+15550130001" {
		t.Fatalf("get = %+v %v", got, err)
	}
	if err := owner.MarkAccountStatus(a.ID, "bogus"); err == nil {
		t.Fatal("bad status allowed")
	}
	if err := owner.MarkAccountStatus(a.ID, AccountDisconnected); err != nil {
		t.Fatalf("status: %v", err)
	}
	if _, err := owner.UpdateAccountLabel(a.ID, "Support Line"); err != nil {
		t.Fatalf("label: %v", err)
	}

	// RLS: foreign org sees nothing; unscoped app sees nothing.
	if err := app.WithOrg(ctx, ob.ID, func(odb *DB) error {
		if _, err := odb.GetAccount(a.ID); !errors.Is(err, sql.ErrNoRows) {
			t.Errorf("cross-org account visible: %v", err)
		}
		if list, err := odb.AccountsByOrg(ob.ID); err != nil || len(list) != 0 {
			t.Errorf("cross-org list = %v %v", list, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("withorg: %v", err)
	}
	if list, err := app.AccountsByOrg(oa.ID); err != nil || len(list) != 0 {
		t.Fatalf("unscoped app list = %v %v", list, err)
	}
	// Home org sees it.
	if err := app.WithOrg(ctx, oa.ID, func(odb *DB) error {
		list, err := odb.AccountsByOrg(oa.ID)
		if err != nil || len(list) != 1 || list[0].ID != a.ID {
			t.Errorf("home list = %v %v", list, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("withorg home: %v", err)
	}

	// Grants cascade on delete; logout clears pairing.
	u, err := owner.CreateUser("multi-acct-grant@example.com", "G", "hash")
	if err != nil {
		t.Fatalf("user: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID) })
	if _, err := owner.AddMembership(u.ID, oa.ID, RoleUser); err != nil {
		t.Fatalf("membership: %v", err)
	}
	if _, err := owner.SetGrant(u.ID, a.ID, oa.ID, "member", "admin-1"); err != nil {
		t.Fatalf("grant: %v", err)
	}
	if err := owner.MarkAccountLoggedOut(a.ID); err != nil {
		t.Fatalf("logout: %v", err)
	}
	cleared, _ := owner.GetAccount(a.ID)
	if cleared.Status != AccountLoggedOut || cleared.DeviceJID != "" {
		t.Fatalf("logout state = %+v", cleared)
	}
	if err := owner.DeleteAccount(a.ID); err != nil {
		t.Fatalf("delete: %v", err)
	}
	if _, err := owner.GetGrant(u.ID, a.ID); !errors.Is(err, sql.ErrNoRows) {
		t.Fatal("grant survived account delete")
	}
}

// TestCampaignAccount covers sender pinning + validation + fallback.
func TestCampaignAccount(t *testing.T) {
	owner, app := umsHandles(t)
	oa, ob := accountTestOrgs(t, owner)
	ctx := context.Background()

	mkContact := func(odb *DB) string {
		c, err := odb.CreateContact("+1555013"+randomDigits(), "T", nil)
		if err != nil {
			t.Fatalf("contact: %v", err)
		}
		t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM contacts WHERE id = ?`, c.ID) })
		return c.ID
	}

	var accAID string
	if err := app.WithOrg(ctx, oa.ID, func(odb *DB) error {
		a, err := odb.CreateAccount(oa.ID, "A-Line", "")
		if err != nil {
			return err
		}
		accAID = a.ID
		cid := mkContact(odb)
		// Explicit valid account pins sender.
		camp, err := odb.CreateCampaign("with-acct", "hi", nil, []string{cid}, "", accAID)
		if err != nil {
			return err
		}
		if camp.WaAccountID != accAID {
			t.Errorf("campaign account = %q", camp.WaAccountID)
		}
		aid, _, err := odb.AccountForCampaign(camp.ID)
		if err != nil || aid != accAID {
			t.Errorf("account for campaign = %q %v", aid, err)
		}
		t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM campaigns WHERE id = ?`, camp.ID) })
		// Unknown account refused.
		if _, err := odb.CreateCampaign("bad-acct", "hi", nil, []string{cid}, "", "no-such-account"); err == nil {
			t.Error("unknown account allowed")
		}
		return nil
	}); err != nil {
		t.Fatalf("org A: %v", err)
	}
	// Cross-org account refused (scoped GetAccount hides it).
	if err := app.WithOrg(ctx, ob.ID, func(odb *DB) error {
		if _, err := odb.CreateAccount(ob.ID, "B-Line", ""); err != nil {
			return err
		}
		cid := mkContact(odb)
		if _, err := odb.CreateCampaign("xorg-acct", "hi", nil, []string{cid}, "", accAID); err == nil {
			t.Error("cross-org account allowed")
		}
		return nil
	}); err != nil {
		t.Fatalf("org B: %v", err)
	}

	// Legacy NULL pin resolves empty (caller falls back to default).
	if err := app.WithOrg(ctx, oa.ID, func(odb *DB) error {
		cid := mkContact(odb)
		camp, err := odb.CreateCampaign("legacy-pin", "hi", nil, []string{cid}, "", "")
		if err != nil {
			return err
		}
		if camp.WaAccountID != "" {
			t.Errorf("legacy pin = %q", camp.WaAccountID)
		}
		aid, _, err := odb.AccountForCampaign(camp.ID)
		if err != nil || aid != "" {
			t.Errorf("legacy resolution = %q %v", aid, err)
		}
		t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM campaigns WHERE id = ?`, camp.ID) })
		return nil
	}); err != nil {
		t.Fatalf("legacy: %v", err)
	}
}

// TestEnsurePairingAccount covers first-pair auto-creation and ambiguity.
func TestEnsurePairingAccount(t *testing.T) {
	owner, _ := umsHandles(t)

	org, err := owner.CreateOrg("Pairing Test Co")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })

	// Zero accounts → creates pending row.
	a, err := owner.EnsurePairingAccount(org.ID, "", "admin-1")
	if err != nil || a.Label != "My WhatsApp number" || a.Status != AccountPending {
		t.Fatalf("auto-create = %+v %v", a, err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM wa_accounts WHERE org_id = ?`, org.ID) })
	// Sole account → reused, no duplicate.
	b, err := owner.EnsurePairingAccount(org.ID, "", "admin-1")
	if err != nil || b.ID != a.ID {
		t.Fatalf("reuse = %+v %v", b, err)
	}
	// Explicit id → validated.
	if _, err := owner.EnsurePairingAccount(org.ID, a.ID, ""); err != nil {
		t.Fatalf("explicit = %v", err)
	}
	if _, err := owner.EnsurePairingAccount(org.ID, "no-such-account", ""); err == nil {
		t.Fatal("unknown explicit id allowed")
	}
	// Second account → ambiguity errors.
	if _, err := owner.CreateAccount(org.ID, "Second", ""); err != nil {
		t.Fatalf("second: %v", err)
	}
	if _, err := owner.EnsurePairingAccount(org.ID, "", ""); err == nil {
		t.Fatal("ambiguous default allowed")
	}
	// Explicit still resolves under ambiguity.
	if _, err := owner.EnsurePairingAccount(org.ID, a.ID, ""); err != nil {
		t.Fatalf("explicit under ambiguity = %v", err)
	}
	// SQLite refuses (no table).
	sqlite, err := OpenSQLite(t.TempDir() + "/pair.db")
	if err != nil {
		t.Fatalf("sqlite: %v", err)
	}
	defer sqlite.Close()
	if _, err := sqlite.EnsurePairingAccount("x", "", ""); err == nil {
		t.Fatal("sqlite allowed")
	}
}

var digitCounter = 10000

func randomDigits() string {
	digitCounter++
	return string(rune('0'+digitCounter/10000%10)) + string(rune('0'+digitCounter/1000%10)) + string(rune('0'+digitCounter/100%10)) + string(rune('0'+digitCounter/10%10)) + string(rune('0'+digitCounter%10))
}
