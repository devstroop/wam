// Package campaigns runs the sender worker: it promotes due
// scheduled campaigns to sending, then dispatches queued recipients in small
// batches with per-message rendering ({{name}}, {{phone}}), pause/cancel
// checks between sends, and receipt-driven delivered/read upgrades.
package campaigns

import (
	"context"
	"log/slog"
	"strings"
	"time"

	"github.com/devstroop/wam/internal/store"
	"github.com/devstroop/wam/internal/wa"
)

// Worker is the only sender (one account → no distributed lock needed).
// app is the runtime handle: every tenant read/write runs inside WithOrg on
// it (fail-closed RLS). owner is privileged and used ONLY for org
// enumeration and msgID→org resolution (receipt routing); on SQLite both are
// the same handle.
type Worker struct {
	app   *store.DB
	owner *store.DB
	wa    *wa.Service
	log   *slog.Logger
	tick  time.Duration
	batch int
	stop  chan struct{}
	done  chan struct{}
}

// New builds the worker (not started).
func New(app, owner *store.DB, w *wa.Service, log *slog.Logger) *Worker {
	if log == nil {
		log = slog.Default()
	}
	return &Worker{app: app, owner: owner, wa: w, log: log, tick: 5 * time.Second, batch: 5, stop: make(chan struct{}), done: make(chan struct{})}
}

// Start launches the loop; Stop blocks until it exits.
func (w *Worker) Start() {
	w.wa.OnReceipt = w.onReceipt
	go w.loop()
}

// Stop halts the worker.
func (w *Worker) Stop() {
	close(w.stop)
	<-w.done
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
			w.runOnceForOrg(odb)
			return nil
		}); err != nil {
			w.log.Warn("campaigns: org run failed", "org", orgID, "err", err)
		}
	}
}

func (w *Worker) runOnceForOrg(db *store.DB) {
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
			}
		}
		return
	}
	for _, r := range recips {
		status, err := db.CampaignStatus(id)
		if err != nil || status != store.CampaignSending {
			return // paused / cancelled / done mid-batch
		}
		w.sendOne(db, id, body, r)
		time.Sleep(time.Second)
	}
}

func (w *Worker) sendOne(db *store.DB, campaignID, body string, r store.Recipient) {
	text := render(body, r.Name, r.Phone)
	jid := wa.JIDForPhone(r.Phone)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	msgID, err := w.wa.SendText(ctx, jid, text)
	if err != nil {
		_ = db.MarkRecipient(campaignID, r.ContactID, store.RecFailed, "", shortErr(err))
		w.log.Warn("campaigns: send failed", "campaign", campaignID, "contact", r.ContactID, "err", err)
		return
	}
	_ = db.MarkRecipient(campaignID, r.ContactID, store.RecSent, msgID, "")
}

// onReceipt upgrades sent → delivered/read by WhatsApp message id. The org is
// resolved via the owner handle first (fail-closed RLS hides the mapping
// from unscoped app handles), then the update runs org-scoped.
func (w *Worker) onReceipt(msgIDs []string, status string) {
	for _, id := range msgIDs {
		orgID, ok := w.owner.OrgForMsgID(id)
		if !ok {
			continue
		}
		if err := w.app.WithOrg(context.Background(), orgID, func(odb *store.DB) error {
			_, _ = odb.MarkByMsgID(id, status)
			return nil
		}); err != nil {
			w.log.Warn("campaigns: receipt update failed", "msg", id, "err", err)
		}
	}
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
