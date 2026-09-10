package store

import (
	"database/sql"
	"fmt"
	"time"
)

// Send queue tuning.
const (
	// MaxAttempts caps delivery tries before a recipient goes failed (DLQ).
	MaxAttempts = 5
	// LockTimeout reclaims jobs from crashed workers.
	LockTimeout = 5 * time.Minute
	// MaxBackoff caps exponential retry delay.
	MaxBackoff = 30 * time.Minute
)

// OutboxJob is one claimed send (contact joined for rendering).
type OutboxJob struct {
	ID         string
	OrgID      string
	CampaignID string
	ContactID  string
	AccountID  string
	Phone      string
	Name       string
	Attempts   int
}

func pgOnly() error { return fmt.Errorf("store: send queue requires postgres") }

// EnqueueCampaign inserts outbox rows for every queued recipient (idempotent
// via the UNIQUE(campaign_id, contact_id) target). Org/account come from the
// campaign row itself, so any scoped handle works. Returns rows inserted.
func (db *DB) EnqueueCampaign(campaignID string) (int64, error) {
	if !db.IsPostgres() {
		return 0, pgOnly()
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(`INSERT INTO send_outbox (id, org_id, campaign_id, contact_id, account_id, next_at)
		SELECT gen_random_uuid()::text, c.org_id, r.campaign_id, r.contact_id, c.wa_account_id, ?
		FROM campaign_recipients r JOIN campaigns c ON c.id = r.campaign_id
		WHERE r.campaign_id = ? AND r.status = 'queued'
		ON CONFLICT (campaign_id, contact_id) DO NOTHING`, now, campaignID)
	if err != nil {
		return 0, err
	}
	return res.RowsAffected()
}

// LaunchCampaign transitions draft/scheduled/paused → sending and enqueues
// the audience atomically from the caller's perspective (both run inside the
// ambient scoped transaction). Single choke point for user Start/Resume and
// worker schedule promotion.
func (db *DB) LaunchCampaign(id string) (*Campaign, error) {
	c, err := db.GetCampaign(id)
	if err != nil {
		return nil, err
	}
	switch c.Status {
	case CampaignDraft, CampaignScheduled, CampaignPaused:
	default:
		return nil, fmt.Errorf("cannot launch from %s", c.Status)
	}
	if _, err := db.SetCampaignStatus(id, CampaignSending); err != nil {
		return nil, err
	}
	if db.IsPostgres() {
		if _, err := db.EnqueueCampaign(id); err != nil {
			return nil, err
		}
	}
	return db.GetCampaign(id)
}

// ClaimJobs locks up to n due jobs for (org, account) — SKIP LOCKED makes
// concurrent workers safe. NULL account pins claim together (legacy rows).
func (db *DB) ClaimJobs(orgID, accountID string, n int, workerID string, now time.Time) ([]OutboxJob, error) {
	if !db.IsPostgres() {
		return nil, pgOnly()
	}
	cutoff := now.Add(-LockTimeout).UTC().Format(time.RFC3339)
	due := now.UTC().Format(time.RFC3339)
	rows, err := db.Query(`UPDATE send_outbox SET locked_by = ?, locked_at = ?
		WHERE id IN (
			SELECT id FROM send_outbox
			WHERE org_id = ? AND account_id IS NOT DISTINCT FROM ?::text
				AND next_at <= ? AND (locked_by = '' OR locked_at < ?)
			ORDER BY created_at, id LIMIT ?
			FOR UPDATE SKIP LOCKED
		)
		RETURNING id, org_id, campaign_id, contact_id, account_id, attempts`,
		workerID, due, orgID, nullIfEmpty(accountID), due, cutoff, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []OutboxJob
	for rows.Next() {
		var j OutboxJob
		var account sql.NullString
		if err := rows.Scan(&j.ID, &j.OrgID, &j.CampaignID, &j.ContactID, &account, &j.Attempts); err != nil {
			return nil, err
		}
		j.AccountID = account.String
		out = append(out, j)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	// Join contact details for rendering (RLS-scoped read).
	for i := range out {
		var phone, name string
		if err := db.QueryRow(`SELECT phone, name FROM contacts WHERE id = ?`, out[i].ContactID).Scan(&phone, &name); err != nil {
			if err == sql.ErrNoRows {
				continue // contact deleted mid-flight; ack below drops it
			}
			return nil, err
		}
		out[i].Phone, out[i].Name = phone, name
	}
	return out, nil
}

// AckJob deletes a finished job (sent, failed-final, or orphaned).
func (db *DB) AckJob(id string) error {
	if !db.IsPostgres() {
		return pgOnly()
	}
	_, err := db.Exec(`DELETE FROM send_outbox WHERE id = ?`, id)
	return err
}

// RetryJob unlocks a job with exponential backoff (1m, 2m, 4m… capped).
func (db *DB) RetryJob(id string, attempts int, now time.Time) error {
	if !db.IsPostgres() {
		return pgOnly()
	}
	next := now.Add(BackoffFor(attempts)).UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE send_outbox SET attempts = ?, next_at = ?, locked_by = '', locked_at = '' WHERE id = ?`,
		attempts, next, id)
	return err
}

// ReleaseJob unlocks one job without touching attempts (pause path).
func (db *DB) ReleaseJob(id string) error {
	if !db.IsPostgres() {
		return pgOnly()
	}
	_, err := db.Exec(`UPDATE send_outbox SET locked_by = '', locked_at = '' WHERE id = ?`, id)
	return err
}

// ReleaseCampaignJobs unlocks all of a campaign's jobs (pause transition).
func (db *DB) ReleaseCampaignJobs(campaignID string) error {
	if !db.IsPostgres() {
		return pgOnly()
	}
	_, err := db.Exec(`UPDATE send_outbox SET locked_by = '', locked_at = '' WHERE campaign_id = ?`, campaignID)
	return err
}

// DropCampaignJobs deletes a campaign's jobs (cancel path).
func (db *DB) DropCampaignJobs(campaignID string) error {
	if !db.IsPostgres() {
		return pgOnly()
	}
	_, err := db.Exec(`DELETE FROM send_outbox WHERE campaign_id = ?`, campaignID)
	return err
}

// OutboxDepth counts due, unlocked jobs (queue-depth metric).
func (db *DB) OutboxDepth(now time.Time) (int, error) {
	if !db.IsPostgres() {
		return 0, pgOnly()
	}
	var n int
	cutoff := now.Add(-LockTimeout).UTC().Format(time.RFC3339)
	due := now.UTC().Format(time.RFC3339)
	if err := db.QueryRow(`SELECT COUNT(*) FROM send_outbox
		WHERE next_at <= ? AND (locked_by = '' OR locked_at < ?)`, due, cutoff).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}

// BackoffFor returns the retry delay after `attempts` failures.
func BackoffFor(attempts int) time.Duration {
	d := time.Minute
	for i := 1; i < attempts && d < MaxBackoff; i++ {
		d *= 2
	}
	if d > MaxBackoff {
		d = MaxBackoff
	}
	return d
}

// SendingCampaigns lists sending campaign ids, oldest first.
func (db *DB) SendingCampaigns() ([]string, error) {
	rows, err := db.Query(`SELECT id FROM campaigns WHERE status = ? ORDER BY created_at, id`, CampaignSending)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			return nil, err
		}
		out = append(out, id)
	}
	return out, rows.Err()
}

// OutboxCountByCampaign counts remaining jobs for a campaign.
func (db *DB) OutboxCountByCampaign(campaignID string) (int, error) {
	var n int
	if err := db.QueryRow(`SELECT COUNT(*) FROM send_outbox WHERE campaign_id = ?`, campaignID).Scan(&n); err != nil {
		return 0, err
	}
	return n, nil
}
