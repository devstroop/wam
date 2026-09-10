// Package campaigns runs the sender worker: it promotes due scheduled
// campaigns to sending, then dispatches queued recipients with per-message
// rendering ({{name}}, {{phone}}), pause/cancel checks, retries with
// backoff, and receipt-driven delivered/read upgrades.
//
// Postgres path: durable send_outbox queue (enqueue on launch, SKIP LOCKED
// claims per org+account, crash-safe locks, DLQ after MaxAttempts).
// SQLite path: legacy direct-recipient loop (unchanged single-tenant flow).
package campaigns

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/devstroop/wam/internal/notify"
	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/wa"
)

// MessageSender delivers one text. *wa.Manager satisfies it; tests fake it.
type MessageSender interface {
	SendText(ctx context.Context, accountID, jid, text string) (string, error)
}

// minSendInterval paces sends per account (Manager's 30/min bucket backstops).
const minSendInterval = time.Second

// Worker is the only sender (single-flight per process; SKIP LOCKED claims
// keep future horizontal workers safe).
// app is the runtime handle: every tenant read/write runs inside WithOrg on
// it (fail-closed RLS). owner is privileged and used ONLY for org
// enumeration and msgID→org resolution (receipt routing); on SQLite both are
// the same handle.
type Worker struct {
	app      *store.DB
	owner    *store.DB
	wa       MessageSender
	notify   *notify.Dispatcher
	log      *slog.Logger
	tick     time.Duration
	batch    int
	workerID string
	stop     chan struct{}
	done     chan struct{}

	paceMu   sync.Mutex
	lastSent map[string]time.Time
}

// New builds the worker (not started). notify may be nil (no fan-out).
// Receipts wire via OnReceipt.
func New(app, owner *store.DB, w MessageSender, notify *notify.Dispatcher, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{
		app: app, owner: owner, wa: w, notify: notify, log: log,
		tick: 5 * time.Second, batch: 10,
		workerID: fmt.Sprintf("%s-%d", hostname(), os.Getpid()),
		stop:     make(chan struct{}), done: make(chan struct{}),
		lastSent: map[string]time.Time{},
	}
}

func hostname() string {
	h, err := os.Hostname()
	if err != nil {
		return "wam"
	}
	return h
}

// Start launches the loop; Stop blocks until it exits.
func (w *Worker) Start() {
	go w.loop()
}

// Stop halts the worker.
func (w *Worker) Stop() {
	close(w.stop)
	<-w.done
}

// OnReceipt upgrades sent → delivered/read by WhatsApp message id. Wire to
// Manager.OnReceipt. The org is resolved via the owner handle first
// (fail-closed RLS hides the mapping from unscoped app handles).
func (w *Worker) OnReceipt(accountID string, msgIDs []string, status string) {
	_ = accountID // msgIDs are device-unique; org scoping below is sufficient
	for _, id := range msgIDs {
		w.onReceipt(id, status)
	}
}

func (w *Worker) onReceipt(msgID, status string) {
	orgID, ok := w.owner.OrgForMsgID(msgID)
	if !ok {
		return
	}
	if err := w.app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
		_, _ = odb.MarkByMsgID(msgID, status)
		w.emit(odb, "message."+status, map[string]any{"message_id": msgID})
		return nil
	}); err != nil {
		w.log.Warn("campaigns: receipt update failed", "msg", msgID, "err", err)
	}
}

// emit fans out org-side (scoped view carries the org). Nil dispatcher safe.
func (w *Worker) emit(db *store.DB, event string, data map[string]any) {
	if w.notify == nil {
		return
	}
	w.notify.Emit(db.OrgID(), event, data)
}

func (w *Worker) loop() {
	defer close(w.done)
	t := time.NewTicker(w.tick)
	defer t.Stop()
	for {
		select {
		case <-w.stop:
			return
		case <-t.C:
			w.runOnce()
		}
	}
}

func (w *Worker) runOnce() {
	orgs, err := w.owner.OrgIDs()
	if err != nil {
		w.log.Warn("campaigns: org list failed", "err", err)
		return
	}
	for _, orgID := range orgs {
		if err := w.app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
			if odb.IsPostgres() {
				w.runQueueForOrg(odb, orgID)
			} else {
				w.runLegacyForOrg(odb)
			}
			return nil
		}); err != nil {
			w.log.Warn("campaigns: org run failed", "org", orgID, "err", err)
		}
	}
}

// --- Postgres queue path ---

func (w *Worker) runQueueForOrg(db *store.DB, orgID string) {
	now := time.Now()
	// Promote due schedules (LaunchCampaign also enqueues the audience).
	due, err := db.DueScheduled(now)
	if err != nil {
		w.log.Warn("campaigns: due query failed", "err", err)
		return
	}
	for _, id := range due {
		if _, err := db.LaunchCampaign(id); err != nil {
			w.log.Warn("campaigns: launch failed", "id", id, "err", err)
			continue
		}
		w.log.Info("campaigns: scheduled -> sending", "id", id)
	}

	// Runtime quota gate: exhausted message quota auto-pauses sending
	// campaigns before any claim, instead of over-sending mid-flight.
	if sending, err := db.SendingCampaigns(); err == nil {
		for _, id := range sending {
			if qerr := db.CheckSendQuota(); qerr != nil {
				w.log.Warn("campaigns: quota exhausted, auto-pausing", "id", id, "err", qerr)
				_, _ = db.SetCampaignStatus(id, store.CampaignPaused)
				_ = db.ReleaseCampaignJobs(id)
			}
		}
	}

	// Accounts to drain: every org account plus the NULL pin (legacy rows).
	accounts, err := db.AccountsByOrg(orgID)
	if err != nil {
		w.log.Warn("campaigns: account list failed", "err", err)
		return
	}
	pins := make([]string, 0, len(accounts)+1)
	for _, a := range accounts {
		pins = append(pins, a.ID)
	}
	pins = append(pins, "") // NULL-pinned legacy rows

	skipped := map[string]bool{} // campaigns paused/cancelled this tick
	for _, pin := range pins {
		jobs, err := db.ClaimJobs(orgID, pin, w.batch, w.workerID, now)
		if err != nil {
			w.log.Warn("campaigns: claim failed", "err", err)
			continue
		}
		for _, job := range jobs {
			if skipped[job.CampaignID] {
				_ = db.ReleaseJob(job.ID)
				continue
			}
			status, err := db.CampaignStatus(job.CampaignID)
			if err != nil || status != store.CampaignSending {
				w.handleStalled(db, job.CampaignID, job.ID, status)
				if status == store.CampaignPaused {
					skipped[job.CampaignID] = true
				}
				continue
			}
			w.sendJob(db, job)
		}
	}

	// Finish sending campaigns with neither queued recipients nor jobs.
	sending, err := db.SendingCampaigns()
	if err != nil {
		return
	}
	for _, id := range sending {
		f, err := db.RecipientFunnel(id)
		if err != nil {
			continue
		}
		if f.Queued != 0 {
			continue
		}
		n, err := db.OutboxCountByCampaign(id)
		if err != nil || n != 0 {
			continue
		}
		if _, err := db.SetCampaignStatus(id, store.CampaignDone); err == nil {
			sent := f.Sent + f.Delivered + f.Read + f.Replied
			w.log.Info("campaigns: sending -> done", "id", id,
				"sent", sent, "failed", f.Failed)
			w.emit(db, "campaign.done", map[string]any{
				"campaign_id": id, "sent": sent, "failed": f.Failed,
			})
		}
	}
}

// handleStalled drops jobs for dead campaigns, releases paused ones.
func (w *Worker) handleStalled(db *store.DB, campaignID, jobID, status string) {
	switch status {
	case store.CampaignPaused:
		// Resume reclaims via lock expiry or explicit release; release now
		// so resume picks up without waiting out LockTimeout.
		_ = db.ReleaseCampaignJobs(campaignID)
	default:
		// Cancelled/done/missing: job is garbage.
		_ = db.AckJob(jobID)
	}
}

func (w *Worker) sendJob(db *store.DB, job store.OutboxJob) {
	accountID := job.AccountID
	if accountID == "" {
		// Legacy pin: sole-account fallback, never a guess.
		accounts, err := db.AccountsByOrg(job.OrgID)
		if err != nil {
			return
		}
		if accountID = store.ResolveDefaultAccount(accounts); accountID == "" {
			w.log.Warn("campaigns: no sendable account", "campaign", job.CampaignID)
			_ = db.ReleaseJob(job.ID)
			return
		}
	}
	if job.Phone == "" {
		// Contact deleted mid-flight: drop.
		_ = db.AckJob(job.ID)
		return
	}
	w.pace(accountID)
	text := render(dbCampaignBody(db, job.CampaignID), job.Name, job.Phone)
	jid := wa.JIDForPhone(job.Phone)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	msgID, err := w.wa.SendText(ctx, accountID, jid, text)
	cancel()
	if err == nil {
		_ = db.MarkRecipient(job.CampaignID, job.ContactID, store.RecSent, msgID, "")
		_ = db.AckJob(job.ID)
		w.emit(db, "message.sent", map[string]any{
			"campaign_id": job.CampaignID, "contact_id": job.ContactID,
			"phone": job.Phone, "account_id": accountID, "message_id": msgID,
		})
		return
	}
	if isPermanent(err) {
		_ = db.MarkRecipient(job.CampaignID, job.ContactID, store.RecFailed, "", shortErr(err))
		_ = db.AckJob(job.ID)
		w.log.Warn("campaigns: send failed (permanent)", "campaign", job.CampaignID, "contact", job.ContactID, "err", err)
		w.emit(db, "message.failed", map[string]any{
			"campaign_id": job.CampaignID, "contact_id": job.ContactID,
			"phone": job.Phone, "account_id": accountID, "error": shortErr(err),
		})
		return
	}
	attempts := job.Attempts + 1
	if attempts >= store.MaxAttempts {
		_ = db.MarkRecipient(job.CampaignID, job.ContactID, store.RecFailed, "", shortErr(err))
		_ = db.AckJob(job.ID)
		w.log.Warn("campaigns: send failed (exhausted)", "campaign", job.CampaignID, "contact", job.ContactID, "err", err)
		w.emit(db, "message.failed", map[string]any{
			"campaign_id": job.CampaignID, "contact_id": job.ContactID,
			"phone": job.Phone, "account_id": accountID, "error": shortErr(err),
		})
		return
	}
	_ = db.RetryJob(job.ID, attempts, time.Now())
	w.log.Warn("campaigns: send failed (retrying)", "campaign", job.CampaignID, "contact", job.ContactID, "attempt", attempts, "err", err)
}

// dbCampaignBody loads the template (warns + empty on failure: send still
// records an attempt rather than wedging the queue).
func dbCampaignBody(db *store.DB, campaignID string) string {
	body, err := db.CampaignBody(campaignID)
	if err != nil {
		return ""
	}
	return body
}

// pace enforces minSendInterval per account (Manager bucket backstops).
func (w *Worker) pace(accountID string) {
	w.paceMu.Lock()
	defer w.paceMu.Unlock()
	if last, ok := w.lastSent[accountID]; ok {
		if wait := minSendInterval - time.Since(last); wait > 0 {
			time.Sleep(wait)
		}
	}
	w.lastSent[accountID] = time.Now()
}

// isPermanent classifies errors that retries cannot heal.
func isPermanent(err error) bool {
	s := err.Error()
	for _, sub := range []string{"not on WhatsApp", "invalid phone", "invalid jid"} {
		if strings.Contains(s, sub) {
			return true
		}
	}
	return false
}

// --- Legacy SQLite path (unchanged single-tenant loop) ---

func (w *Worker) runLegacyForOrg(db *store.DB) {
	now := time.Now()
	due, err := db.DueScheduled(now)
	if err != nil {
		w.log.Warn("campaigns: due query failed", "err", err)
		return
	}
	for _, id := range due {
		if _, err := db.SetCampaignStatus(id, store.CampaignSending); err != nil {
			w.log.Warn("campaigns: promote failed", "id", id, "err", err)
			continue
		}
		w.log.Info("campaigns: scheduled -> sending", "id", id)
	}

	id, ok := db.NextSending()
	if !ok {
		return
	}
	body, err := db.CampaignBody(id)
	if err != nil {
		w.log.Warn("campaigns: body load failed", "id", id, "err", err)
		return
	}
	recips, err := db.ClaimRecipients(id, w.batch)
	if err != nil {
		w.log.Warn("campaigns: claim failed", "id", id, "err", err)
		return
	}
	if len(recips) == 0 {
		f, err := db.RecipientFunnel(id)
		if err != nil {
			return
		}
		if f.Queued == 0 {
			if _, err := db.SetCampaignStatus(id, store.CampaignDone); err == nil {
				w.log.Info("campaigns: sending -> done", "id", id,
					"sent", f.Sent+f.Delivered+f.Read+f.Replied, "failed", f.Failed)
				w.emit(db, "campaign.done", map[string]any{
					"campaign_id": id,
					"sent":        f.Sent + f.Delivered + f.Read + f.Replied,
					"failed":      f.Failed,
				})
			}
		}
		return
	}
	for _, r := range recips {
		status, err := db.CampaignStatus(id)
		if err != nil || status != store.CampaignSending {
			return // paused / cancelled / done mid-batch
		}
		w.sendLegacy(db, id, body, r)
		time.Sleep(time.Second)
	}
}

func (w *Worker) sendLegacy(db *store.DB, campaignID, body string, r store.Recipient) {
	text := render(body, r.Name, r.Phone)
	jid := wa.JIDForPhone(r.Phone)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgID, err := w.wa.SendText(ctx, store.LegacyAccountID, jid, text)
	if err != nil {
		_ = db.MarkRecipient(campaignID, r.ContactID, store.RecFailed, "", shortErr(err))
		w.log.Warn("campaigns: send failed", "campaign", campaignID, "contact", r.ContactID, "err", err)
		w.emit(db, "message.failed", map[string]any{
			"campaign_id": campaignID, "contact_id": r.ContactID,
			"phone": r.Phone, "account_id": store.LegacyAccountID, "error": shortErr(err),
		})
		return
	}
	_ = db.MarkRecipient(campaignID, r.ContactID, store.RecSent, msgID, "")
	w.emit(db, "message.sent", map[string]any{
		"campaign_id": campaignID, "contact_id": r.ContactID,
		"phone": r.Phone, "account_id": store.LegacyAccountID, "message_id": msgID,
	})
}

// render substitutes {{name}} and {{phone}} (unknown vars left as-is).
func render(body, name, phone string) string {
	if strings.TrimSpace(name) == "" {
		name = phone
	}
	out := strings.ReplaceAll(body, "{{name}}", name)
	return strings.ReplaceAll(out, "{{phone}}", phone)
}

func shortErr(err error) string {
	s := err.Error()
	if len(s) > 200 {
		return s[:200]
	}
	return s
}
