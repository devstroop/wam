package campaigns

import (
	"context"
	"errors"
	"fmt"
	"os"
	"sync"
	"testing"
	"time"

	"github.com/devstroop/wam/internal/store"
)

// fakeSender fakes delivery: fail remaining times with failErr, else success.
type fakeSender struct {
	mu       sync.Mutex
	calls    int
	failLeft int
	failErr  error
	accounts map[string]int
}

func (f *fakeSender) SendText(ctx context.Context, accountID, jid, text string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	if f.accounts == nil {
		f.accounts = map[string]int{}
	}
	f.accounts[accountID]++
	if f.failLeft > 0 {
		f.failLeft--
		return "", f.failErr
	}
	return fmt.Sprintf("fake-msg-%d", f.calls), nil
}

func testHandles(t *testing.T) (app, owner *store.DB) {
	t.Helper()
	ownerDSN := envOrSkip(t, "WAM_DATABASE_URL")
	appDSN := envOrSkip(t, "WAM_APP_DATABASE_URL")
	var err error
	owner, err = store.OpenPostgres(ownerDSN)
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	app, err = store.OpenPostgresNoMigrate(appDSN)
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	return owner, app
}

func envOrSkip(t *testing.T, key string) string {
	t.Helper()
	v := os.Getenv(key)
	if v == "" {
		t.Skip(key + " unset")
	}
	return v
}

var workerPhoneSeq = 20000

// workerFixture builds org + contact + account, cleaned up afterwards.
// Returns scoped runner bound to the org.
func workerFixture(t *testing.T, owner, app *store.DB, nContacts int) (orgID, accountID string, contactIDs []string) {
	t.Helper()
	ctx := context.Background()
	org, err := owner.CreateOrg("Worker Test Co")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })
	var accountIDOut string
	if err := app.WithOrg(ctx, org.ID, func(odb *store.DB) error {
		a, err := odb.CreateAccount(org.ID, "Test Line", "")
		if err != nil {
			return err
		}
		accountIDOut = a.ID
		for i := 0; i < nContacts; i++ {
			workerPhoneSeq++
			c, err := odb.CreateContact(fmt.Sprintf("+1555014%05d", workerPhoneSeq), "W", nil)
			if err != nil {
				return err
			}
			contactIDs = append(contactIDs, c.ID)
		}
		return nil
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() {
		for _, cid := range contactIDs {
			_, _ = owner.Exec(`DELETE FROM contacts WHERE id = ?`, cid)
		}
		_, _ = owner.Exec(`DELETE FROM wa_accounts WHERE id = ?`, accountIDOut)
	})
	return org.ID, accountIDOut, contactIDs
}

func launchFixtureCampaign(t *testing.T, app *store.DB, orgID, accountID string, contactIDs []string) string {
	t.Helper()
	var campID string
	if err := app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		camp, err := odb.CreateCampaign("worker-test", "hi {{name}}", nil, contactIDs, "", accountID)
		if err != nil {
			return err
		}
		campID = camp.ID
		if _, err := odb.LaunchCampaign(camp.ID); err != nil {
			return err
		}
		return nil
	}); err != nil {
		t.Fatalf("launch: %v", err)
	}
	t.Cleanup(func() {
		_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
			_ = odb.DeleteCampaign(campID)
			return nil
		})
	})
	return campID
}

func newTestWorker(app, owner *store.DB, sender MessageSender) *Worker {
	w := New(app, owner, sender, nil, nil)
	w.tick = time.Hour // manual ticks only
	w.batch = 10
	return w
}

// TestWorkerDrainToDone sends everything and finishes the campaign.
func TestWorkerDrainToDone(t *testing.T) {
	owner, app := testHandles(t)
	orgID, accountID, contacts := workerFixture(t, owner, app, 3)
	campID := launchFixtureCampaign(t, app, orgID, accountID, contacts)

	fake := &fakeSender{}
	w := newTestWorker(app, owner, fake)
	w.runOnce()
	w.runOnce() // second pass finishes (no jobs left)

	if fake.calls != 3 {
		t.Fatalf("sends = %d, want 3", fake.calls)
	}
	if fake.accounts[accountID] != 3 {
		t.Fatalf("account sends = %v", fake.accounts)
	}
	var status string
	_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		var err error
		status, err = odb.CampaignStatus(campID)
		return err
	})
	if status != store.CampaignDone {
		t.Fatalf("status = %q", status)
	}
	var remaining int
	_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		var err error
		remaining, err = odb.OutboxCountByCampaign(campID)
		return err
	})
	if remaining != 0 {
		t.Fatalf("outbox remaining = %d", remaining)
	}
}

// TestWorkerRetryThenSuccess fails twice transiently, then delivers.
func TestWorkerRetryThenSuccess(t *testing.T) {
	owner, app := testHandles(t)
	orgID, accountID, contacts := workerFixture(t, owner, app, 1)
	campID := launchFixtureCampaign(t, app, orgID, accountID, contacts)

	fake := &fakeSender{failLeft: 2, failErr: errors.New("wa: send: timeout")}
	w := newTestWorker(app, owner, fake)
	ctx := context.Background()

	w.runOnce()
	if fake.calls != 1 {
		t.Fatalf("calls = %d", fake.calls)
	}
	// Job backing off, not due: second pass sends nothing.
	w.runOnce()
	if fake.calls != 1 {
		t.Fatalf("retry sent early, calls = %d", fake.calls)
	}
	// Force due twice more.
	for i := 0; i < 2; i++ {
		if _, err := owner.Exec(`UPDATE send_outbox SET next_at = '2000-01-01T00:00:00Z', locked_by = '', locked_at = '' WHERE campaign_id = ?`, campID); err != nil {
			t.Fatalf("force due: %v", err)
		}
		w.runOnce()
	}
	if fake.calls != 3 {
		t.Fatalf("calls = %d, want 3", fake.calls)
	}
	var status string
	_ = app.WithOrg(ctx, orgID, func(odb *store.DB) error {
		f, err := odb.RecipientFunnel(campID)
		if err != nil {
			return err
		}
		if f.Sent != 1 || f.Failed != 0 {
			t.Errorf("funnel = %+v", f)
		}
		var err2 error
		status, err2 = odb.CampaignStatus(campID)
		return err2
	})
	if status != store.CampaignDone {
		t.Fatalf("status = %q", status)
	}
}

// TestWorkerPermanentFailure fails fast without retries.
func TestWorkerPermanentFailure(t *testing.T) {
	owner, app := testHandles(t)
	orgID, accountID, contacts := workerFixture(t, owner, app, 1)
	campID := launchFixtureCampaign(t, app, orgID, accountID, contacts)

	fake := &fakeSender{failLeft: 100, failErr: errors.New("phone +15550140000 is not on WhatsApp")}
	w := newTestWorker(app, owner, fake)
	w.runOnce()
	if fake.calls != 1 {
		t.Fatalf("calls = %d, want exactly 1 (no retry)", fake.calls)
	}
	_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		f, err := odb.RecipientFunnel(campID)
		if err != nil {
			return err
		}
		if f.Failed != 1 || f.Queued != 0 {
			t.Errorf("funnel = %+v", f)
		}
		n, err := odb.OutboxCountByCampaign(campID)
		if err != nil || n != 0 {
			t.Errorf("outbox = %d %v", n, err)
		}
		return nil
	})
}

// TestWorkerPauseReleases keeps jobs queued+unlocked while paused.
func TestWorkerPauseReleases(t *testing.T) {
	owner, app := testHandles(t)
	orgID, accountID, contacts := workerFixture(t, owner, app, 2)
	campID := launchFixtureCampaign(t, app, orgID, accountID, contacts)

	fake := &fakeSender{}
	w := newTestWorker(app, owner, fake)
	// Pause before any tick (handler-equivalent queue ops).
	_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		if _, err := odb.SetCampaignStatus(campID, store.CampaignPaused); err != nil {
			return err
		}
		return odb.ReleaseCampaignJobs(campID)
	})
	w.runOnce()
	if fake.calls != 0 {
		t.Fatalf("paused campaign sent %d", fake.calls)
	}
	_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		f, err := odb.RecipientFunnel(campID)
		if err != nil {
			return err
		}
		if f.Queued != 2 {
			t.Errorf("queued = %d, want 2 retained", f.Queued)
		}
		n, err := odb.OutboxCountByCampaign(campID)
		if err != nil || n != 2 {
			t.Errorf("outbox = %d %v, want 2 retained", n, err)
		}
		// Jobs unlocked (claimable on resume).
		var locked int
		if err := odb.QueryRow(`SELECT COUNT(*) FROM send_outbox WHERE campaign_id = ? AND locked_by != ''`, campID).Scan(&locked); err != nil || locked != 0 {
			t.Errorf("locked = %d %v, want 0", locked, err)
		}
		status, err := odb.CampaignStatus(campID)
		if err != nil || status != store.CampaignPaused {
			t.Errorf("status = %q %v", status, err)
		}
		return nil
	})
}

// TestWorkerCancelDrops discards jobs without sending.
func TestWorkerCancelDrops(t *testing.T) {
	owner, app := testHandles(t)
	orgID, accountID, contacts := workerFixture(t, owner, app, 2)
	campID := launchFixtureCampaign(t, app, orgID, accountID, contacts)

	fake := &fakeSender{}
	w := newTestWorker(app, owner, fake)
	_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		if _, err := odb.SetCampaignStatus(campID, store.CampaignCancelled); err != nil {
			return err
		}
		return odb.DropCampaignJobs(campID)
	})
	w.runOnce()
	if fake.calls != 0 {
		t.Fatalf("cancelled campaign sent %d", fake.calls)
	}
	_ = app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		status, err := odb.CampaignStatus(campID)
		if err != nil || status != store.CampaignCancelled {
			t.Errorf("status = %q %v", status, err)
		}
		return nil
	})
}

// TestBackoffFor checks retry delays (1m, 2m, 4m … capped at 30m).
func TestBackoffFor(t *testing.T) {
	if got := store.BackoffFor(1); got != time.Minute {
		t.Fatalf("attempt 1 = %v", got)
	}
	if got := store.BackoffFor(3); got != 4*time.Minute {
		t.Fatalf("attempt 3 = %v", got)
	}
	if got := store.BackoffFor(99); got != 30*time.Minute {
		t.Fatalf("cap = %v", got)
	}
}

// TestIsPermanent checks error classification.
func TestIsPermanent(t *testing.T) {
	if !isPermanent(errors.New("phone +1 is not on WhatsApp")) {
		t.Fatal("not-on-WA should be permanent")
	}
	if !isPermanent(errors.New("wa: invalid jid")) {
		t.Fatal("bad JID should be permanent")
	}
	if isPermanent(errors.New("wa: send: timeout")) {
		t.Fatal("timeout should retry")
	}
}
