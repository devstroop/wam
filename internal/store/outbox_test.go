package store

import (
	"context"
	"os"
	"testing"
	"time"
)

// TestOutboxEnqueueClaim covers idempotent enqueue, locking, release, drop.
func TestOutboxEnqueueClaim(t *testing.T) {
	dsn := mustPG(t)
	owner, err := OpenPostgres(dsn)
	if err != nil {
		t.Fatalf("open owner: %v", err)
	}
	t.Cleanup(func() { _ = owner.Close() })
	appDSN := mustAppPG(t)
	app, err := OpenPostgresNoMigrate(appDSN)
	if err != nil {
		t.Fatalf("open app: %v", err)
	}
	t.Cleanup(func() { _ = app.Close() })
	ctx := context.Background()

	org, err := owner.CreateOrg("Outbox Test Co")
	if err != nil {
		t.Fatalf("org: %v", err)
	}
	t.Cleanup(func() { _, _ = owner.Exec(`DELETE FROM orgs WHERE id = ?`, org.ID) })

	var campID, contactID string
	if err := app.WithOrg(ctx, org.ID, func(odb *DB) error {
		c, err := odb.CreateContact("+15550159001", "Q", nil)
		if err != nil {
			return err
		}
		contactID = c.ID
		camp, err := odb.CreateCampaign("queue-test", "hi", nil, []string{c.ID}, "", "")
		if err != nil {
			return err
		}
		campID = camp.ID
		return nil
	}); err != nil {
		t.Fatalf("fixture: %v", err)
	}
	t.Cleanup(func() {
		_, _ = owner.Exec(`DELETE FROM campaigns WHERE id = ?`, campID)
		_, _ = owner.Exec(`DELETE FROM contacts WHERE id = ?`, contactID)
	})

	enq := func() int64 {
		var n int64
		_ = app.WithOrg(ctx, org.ID, func(odb *DB) error {
			var err error
			n, err = odb.EnqueueCampaign(campID)
			return err
		})
		return n
	}
	if n := enq(); n != 1 {
		t.Fatalf("enqueue = %d, want 1", n)
	}
	if n := enq(); n != 0 {
		t.Fatalf("re-enqueue = %d, want 0 (idempotent)", n)
	}

	var depth int
	_ = app.WithOrg(ctx, org.ID, func(odb *DB) error {
		var err error
		depth, err = odb.OutboxDepth(time.Now())
		return err
	})
	if depth != 1 {
		t.Fatalf("depth = %d, want 1", depth)
	}

	// Claim locks; second claim sees nothing (SKIP LOCKED / locked_by set).
	var jobID string
	if err := app.WithOrg(ctx, org.ID, func(odb *DB) error {
		jobs, err := odb.ClaimJobs(org.ID, "", 10, "w1", time.Now())
		if err != nil || len(jobs) != 1 {
			t.Fatalf("claim = %v %v", jobs, err)
		}
		jobID = jobs[0].ID
		if jobs[0].Phone != "+15550159001" {
			t.Fatalf("job phone = %q", jobs[0].Phone)
		}
		again, err := odb.ClaimJobs(org.ID, "", 10, "w2", time.Now())
		if err != nil || len(again) != 0 {
			t.Fatalf("second claim = %v %v (lock not held)", again, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("claim: %v", err)
	}

	// Release unlocks; retry sets backoff; drop removes.
	if err := app.WithOrg(ctx, org.ID, func(odb *DB) error {
		if err := odb.ReleaseJob(jobID); err != nil {
			return err
		}
		jobs, err := odb.ClaimJobs(org.ID, "", 10, "w2", time.Now())
		if err != nil || len(jobs) != 1 {
			t.Fatalf("reclaim = %v %v", jobs, err)
		}
		if err := odb.RetryJob(jobID, 1, time.Now()); err != nil {
			return err
		}
		jobs, err = odb.ClaimJobs(org.ID, "", 10, "w2", time.Now())
		if err != nil || len(jobs) != 0 {
			t.Fatalf("backoff claim = %v %v (should wait)", jobs, err)
		}
		if err := odb.DropCampaignJobs(campID); err != nil {
			return err
		}
		n, err := odb.OutboxCountByCampaign(campID)
		if err != nil || n != 0 {
			t.Fatalf("after drop = %d %v", n, err)
		}
		return nil
	}); err != nil {
		t.Fatalf("release/retry/drop: %v", err)
	}
}

func mustPG(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("WAM_DATABASE_URL"); v != "" {
		return v
	}
	t.Skip("WAM_DATABASE_URL unset")
	return ""
}

func mustAppPG(t *testing.T) string {
	t.Helper()
	if v := os.Getenv("WAM_APP_DATABASE_URL"); v != "" {
		return v
	}
	t.Skip("WAM_APP_DATABASE_URL unset")
	return ""
}
