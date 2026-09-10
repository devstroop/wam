package campaigns

import (
	"context"
	"os"
	"testing"

	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/wa"
)

// TestWorkerQuotaAutoPause forces a sending campaign on a zero-message plan:
// the worker must pause it without attempting any send.
func TestWorkerQuotaAutoPause(t *testing.T) {
	ownerDSN := os.Getenv("WAM_DATABASE_URL")
	appDSN := os.Getenv("WAM_APP_DATABASE_URL")
	if ownerDSN == "" || appDSN == "" {
		t.Skip("PG env unset")
	}
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
	ctx := context.Background()

	if _, err := owner.Exec(`INSERT INTO plans (id, code, name, limits, price_paise)
		VALUES ('plan_pause_test', 'pause_test', 'Pause Test', '{"accounts":1,"contacts":10,"msgs_per_month":0,"members":5}', 0)
		ON CONFLICT (id) DO NOTHING`); err != nil {
		t.Fatalf("seed plan: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM plans WHERE id = 'plan_pause_test'`) })
	org, err := owner.CreateOrg("Quota Pause Co")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })
	if _, err := owner.SetSubscription(org.ID, "pause_test", "active", "", "", ""); err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	var campID string
	if err := app.WithOrg(ctx, org.ID, func(odb *store.DB) error {
		if _, err := odb.CreateAccount(org.ID, "Q-Line", ""); err != nil {
			return err
		}
		c, err := odb.CreateContact("+15550169201", "P", nil)
		if err != nil {
			return err
		}
		camp, err := odb.CreateCampaign("quota-pause", "hi", nil, []string{c.ID}, "", "")
		if err != nil {
			return err
		}
		campID = camp.ID
		// Bypass the handler Start check: force sending to test the worker gate.
		_, err = odb.SetCampaignStatus(camp.ID, store.CampaignSending)
		return err
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM campaigns WHERE id = ?`, campID) })
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM contacts WHERE org_id = ?`, org.ID) })

	w := New(app, owner, wa.NewManager("", "", false, nil), nil)
	w.tick = 0 // unused; direct runOnce
	w.runOnce()

	_ = app.WithOrg(ctx, org.ID, func(odb *store.DB) error {
		status, err := odb.CampaignStatus(campID)
		if err != nil {
			return err
		}
		if status != store.CampaignPaused {
			t.Errorf("status = %q, want paused", status)
		}
		f, err := odb.RecipientFunnel(campID)
		if err != nil {
			return err
		}
		if f.Queued != 1 || f.Sent+f.Failed != 0 {
			t.Errorf("funnel = %+v, want 1 queued untouched", f)
		}
		return nil
	})
}
