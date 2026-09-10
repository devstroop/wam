package store

import (
	"context"
	"errors"
	"testing"
)

// TestBillingPlans covers seed tiers + effective default.
func TestBillingPlans(t *testing.T) {
	owner, _ := umsHandles(t)

	plans, err := owner.ListPlans()
	if err != nil || len(plans) != 4 {
		t.Fatalf("plans = %v %v", plans, err)
	}
	free, err := owner.GetPlan("free")
	if err != nil || free.Limits.Contacts != 500 || free.Limits.MsgsPerMonth != 1000 {
		t.Fatalf("free = %+v %v", free, err)
	}
	if _, err := owner.GetPlan("nope"); err == nil {
		t.Fatal("unknown plan found")
	}

	org, err := owner.CreateOrg("Billing Test Co")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })

	// No subscription row → free.
	eff, err := owner.EffectivePlan(org.ID)
	if err != nil || eff.Code != "free" {
		t.Fatalf("effective = %+v %v", eff, err)
	}
	if err := owner.CheckQuota(org.ID, "contact"); err != nil {
		t.Fatalf("fresh org blocked: %v", err)
	}
	if err := owner.CheckQuota(org.ID, "bogus"); err == nil {
		t.Fatal("bad kind allowed")
	}
}

// TestBillingQuotas crafts a tiny plan to exercise every gate.
func TestBillingQuotas(t *testing.T) {
	owner, app := umsHandles(t)

	if _, err := owner.Exec(`INSERT INTO plans (id, code, name, limits, price_paise)
		VALUES ('plan_tiny_test', 'tiny_test', 'Tiny Test', '{"accounts":1,"contacts":1,"msgs_per_month":0,"members":1}', 0)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed tiny: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM plans WHERE id = 'plan_tiny_test'`) })

	org, err := owner.CreateOrg("Quota Test Co")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })
	if _, err := owner.SetSubscription(org.ID, "tiny_test", "active", "", "", ""); err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	eff, _ := owner.EffectivePlan(org.ID)
	if eff.Code != "tiny_test" {
		t.Fatalf("effective = %s", eff.Code)
	}
	// Inactive status falls back to free.
	if _, err := owner.SetSubscription(org.ID, "tiny_test", "past_due", "", "", ""); err != nil {
		t.Fatalf("status: %v", err)
	}
	if eff, _ := owner.EffectivePlan(org.ID); eff.Code != "free" {
		t.Fatalf("past_due effective = %s", eff.Code)
	}
	if _, err := owner.SetSubscription(org.ID, "tiny_test", "active", "", "", ""); err != nil {
		t.Fatalf("reactivate: %v", err)
	}
	if _, err := owner.SetSubscription(org.ID, "nope", "active", "", "", ""); err == nil {
		t.Fatal("unknown plan subscribed")
	}

	// Contacts: 1 allowed, 2nd blocked.
	var c1 string
	var checkErr error
	if err := app.WithOrg(ctxOf(t), org.ID, func(odb *DB) error {
		c, err := odb.CreateContact("+15550169001", "Q1", nil)
		if err != nil {
			return err
		}
		c1 = c.ID
		checkErr = odb.CheckQuota(org.ID, "contact")
		return nil
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	if checkErr == nil {
		t.Fatal("second contact allowed")
	} else {
		var qe *QuotaError
		if !errors.As(checkErr, &qe) || qe.Limit != 1 {
			t.Fatalf("err = %v", checkErr)
		}
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM contacts WHERE id = ?`, c1) })

	// Messages: tiny allows 0 → any send blocked.
	if err := app.WithOrg(ctxOf(t), org.ID, func(odb *DB) error {
		return odb.CheckQuota(org.ID, "message")
	}); err == nil {
		t.Fatal("message allowed on zero quota")
	}

	// Members: 1 slot used by admin below → next blocked.
	u, _ := owner.CreateUser("quota-admin@example.com", "QA", "hash")
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM users WHERE id = ?`, u.ID) })
	if _, err := owner.AddMembership(u.ID, org.ID, RoleAdmin); err != nil {
		t.Fatalf("membership: %v", err)
	}
	if err := owner.CheckQuota(org.ID, "member"); err == nil {
		t.Fatal("member over limit allowed")
	}

	// Usage derivation sanity.
	usage, err := owner.UsageForOrg(org.ID)
	if err != nil || usage.Contacts != 1 || usage.Members != 1 || usage.Period == "" {
		t.Fatalf("usage = %+v %v", usage, err)
	}

	// Invoices.
	inv, err := owner.RecordInvoice(org.ID, "manual", "inv-1", 49900, "", "")
	if err != nil || inv.Currency != "INR" || inv.Status != "open" {
		t.Fatalf("invoice = %+v %v", inv, err)
	}
	invs, err := owner.InvoicesForOrg(org.ID)
	if err != nil || len(invs) != 1 || invs[0].AmountPaise != 49900 {
		t.Fatalf("invoices = %+v %v", invs, err)
	}
}

func ctxOf(t *testing.T) context.Context {
	t.Helper()
	return context.Background()
}
