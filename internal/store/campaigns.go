package store

import (
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Campaign statuses.
const (
	CampaignDraft     = "draft"
	CampaignScheduled = "scheduled"
	CampaignSending   = "sending"
	CampaignPaused    = "paused"
	CampaignDone      = "done"
	CampaignCancelled = "cancelled"
)

// Recipient statuses.
const (
	RecQueued    = "queued"
	RecSent      = "sent"
	RecDelivered = "delivered"
	RecRead      = "read"
	RecReplied   = "replied"
	RecFailed    = "failed"
)

// Campaign is a broadcast to an audience snapshot (groups + individual contacts).
type Campaign struct {
	ID           string   `json:"id"`
	Name         string   `json:"name"`
	Status       string   `json:"status"`
	BodyTemplate string   `json:"bodyTemplate"`
	GroupIDs     []string `json:"groupIds,omitempty"`
	ContactIDs   []string `json:"contactIds,omitempty"`
	ScheduledAt  string   `json:"scheduledAt,omitempty"`
	WaAccountID  string   `json:"waAccountId,omitempty"`
	CreatedAt    string   `json:"createdAt"`
	Total        int      `json:"total"`
	Queued       int      `json:"queued"`
	Sent         int      `json:"sent"`
	Failed       int      `json:"failed"`
}

// Recipient is one per-contact delivery state.
type Recipient struct {
	CampaignID string `json:"campaignId"`
	ContactID  string `json:"contactId"`
	Phone      string `json:"phone"`
	Name       string `json:"name,omitempty"`
	Status     string `json:"status"`
	WAMsgID    string `json:"waMsgId,omitempty"`
	Error      string `json:"error,omitempty"`
	SentAt     string `json:"sentAt,omitempty"`
}

// Funnel aggregates recipient statuses.
type Funnel struct {
	Queued    int `json:"queued"`
	Sent      int `json:"sent"`
	Delivered int `json:"delivered"`
	Read      int `json:"read"`
	Replied   int `json:"replied"`
	Failed    int `json:"failed"`
}

func (f Funnel) Total() int {
	return f.Queued + f.Sent + f.Delivered + f.Read + f.Replied + f.Failed
}

// CreateCampaign inserts a draft/scheduled campaign and snapshots the audience.
// Empty groupIDs+contactIDs targets ALL contacts. waAccountID pins the sender
// ("" = legacy/unset; handlers resolve the default before calling).
// On Postgres a non-empty account must belong to the caller's org.
func (db *DB) CreateCampaign(name, bodyTemplate string, groupIDs, contactIDs []string, scheduledAt, waAccountID string) (*Campaign, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 200 {
		return nil, fmt.Errorf("invalid campaign name")
	}
	if strings.TrimSpace(bodyTemplate) == "" {
		return nil, fmt.Errorf("invalid message template")
	}
	if waAccountID != "" && db.IsPostgres() {
		if _, err := db.GetAccount(waAccountID); err != nil {
			return nil, fmt.Errorf("unknown account %q", waAccountID)
		}
	}
	for _, gid := range groupIDs {
		if _, err := db.GetGroup(gid); err != nil {
			return nil, fmt.Errorf("unknown group %q", gid)
		}
	}
	for _, cid := range contactIDs {
		if _, err := db.GetContact(cid); err != nil {
			return nil, fmt.Errorf("unknown contact %q", cid)
		}
	}
	status := CampaignDraft
	if strings.TrimSpace(scheduledAt) != "" {
		if _, err := time.Parse(time.RFC3339, scheduledAt); err != nil {
			return nil, fmt.Errorf("invalid scheduledAt (RFC3339)")
		}
		status = CampaignScheduled
	}
	aud, _ := json.Marshal(map[string]any{"group_ids": groupIDs, "contact_ids": contactIDs})
	c := &Campaign{ID: uuid.NewString()}
	tx, err := db.Begin()
	if err != nil {
		return nil, err
	}
	defer tx.Rollback()
	if db.IsPostgres() {
		_, err = tx.Exec(`INSERT INTO campaigns (id, name, status, body_template, audience_filter, scheduled_at, org_id, wa_account_id)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?)`, c.ID, name, status, bodyTemplate, string(aud), strings.TrimSpace(scheduledAt), db.orgOr(DefaultOrgID), nullIfEmpty(waAccountID))
	} else {
		_, err = tx.Exec(`INSERT INTO campaigns (id, name, status, body_template, audience_filter, scheduled_at)
			VALUES (?, ?, ?, ?, ?, ?)`, c.ID, name, status, bodyTemplate, string(aud), strings.TrimSpace(scheduledAt))
	}
	if err != nil {
		return nil, err
	}
	// Resolve audience: union of groups + explicit contacts; empty = all
	var contactIDsResolved []string
	seen := make(map[string]struct{})
	if len(groupIDs) == 0 && len(contactIDs) == 0 {
		rows, err := tx.Query(`SELECT id FROM contacts`)
		if err != nil {
			return nil, err
		}
		for rows.Next() {
			var id string
			if err := rows.Scan(&id); err != nil {
				rows.Close()
				return nil, err
			}
			if _, ok := seen[id]; !ok {
				seen[id] = struct{}{}
				contactIDsResolved = append(contactIDsResolved, id)
			}
		}
		rows.Close()
	} else {
		if len(groupIDs) > 0 {
			q := `SELECT DISTINCT cg.contact_id FROM contact_groups cg WHERE cg.group_id IN (` + placeholders(len(groupIDs)) + `)`
			args := make([]any, len(groupIDs))
			for i, g := range groupIDs {
				args[i] = g
			}
			rows, err := tx.Query(q, args...)
			if err != nil {
				return nil, err
			}
			for rows.Next() {
				var id string
				if err := rows.Scan(&id); err != nil {
					rows.Close()
					return nil, err
				}
				if _, ok := seen[id]; !ok {
					seen[id] = struct{}{}
					contactIDsResolved = append(contactIDsResolved, id)
				}
			}
			rows.Close()
		}
		for _, cid := range contactIDs {
			if _, ok := seen[cid]; !ok {
				seen[cid] = struct{}{}
				contactIDsResolved = append(contactIDsResolved, cid)
			}
		}
	}
	contactIDs = contactIDsResolved
	orgID := db.orgOr(DefaultOrgID)
	for _, cid := range contactIDs {
		if db.IsPostgres() {
			_, err := tx.Exec(`INSERT INTO campaign_recipients (campaign_id, contact_id, org_id) VALUES (?, ?, ?) ON CONFLICT DO NOTHING`, c.ID, cid, orgID)
			if err != nil {
				return nil, err
			}
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO campaign_recipients (campaign_id, contact_id) VALUES (?, ?)`, c.ID, cid); err != nil {
			return nil, err
		}
	}
	if err := tx.Commit(); err != nil {
		return nil, err
	}
	return db.GetCampaign(c.ID)
}

func placeholders(n int) string {
	s := make([]string, n)
	for i := range s {
		s[i] = "?"
	}
	return strings.Join(s, ",")
}

// nullIfEmpty maps "" to nil so optional FK columns store NULL, not ”.
func nullIfEmpty(s string) any {
	if s == "" {
		return nil
	}
	return s
}

// GetCampaign fetches one campaign with recipient counters.
func (db *DB) GetCampaign(id string) (*Campaign, error) {
	c := &Campaign{}
	var aud string
	var waAccount sql.NullString
	err := db.QueryRow(`SELECT id, name, status, body_template, audience_filter, scheduled_at, created_at
		FROM campaigns WHERE id = ?`, id).Scan(&c.ID, &c.Name, &c.Status, &c.BodyTemplate, &aud, &c.ScheduledAt, &c.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	if db.IsPostgres() {
		// wa_account_id lives only on Postgres (009); read separately to
		// keep the shared query above working on SQLite.
		_ = db.QueryRow(`SELECT wa_account_id FROM campaigns WHERE id = ?`, id).Scan(&waAccount)
		c.WaAccountID = waAccount.String
	}
	var f struct {
		GroupIDs   []string `json:"group_ids"`
		ContactIDs []string `json:"contact_ids"`
	}
	_ = json.Unmarshal([]byte(aud), &f)
	c.GroupIDs = f.GroupIDs
	c.ContactIDs = f.ContactIDs
	funnel, err := db.RecipientFunnel(id)
	if err != nil {
		return nil, err
	}
	c.Total = funnel.Total()
	c.Queued = funnel.Queued
	c.Sent = funnel.Sent + funnel.Delivered + funnel.Read + funnel.Replied
	c.Failed = funnel.Failed
	return c, nil
}

// ListCampaigns paginates newest-first.
func (db *DB) ListCampaigns(limit int, cursor string) ([]Campaign, string, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "%d", &offset)
		if offset < 0 {
			offset = 0
		}
	}
	rows, err := db.Query(`SELECT id FROM campaigns ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`, limit+1, offset)
	if err != nil {
		return nil, "", err
	}
	var ids []string
	for rows.Next() {
		var id string
		if err := rows.Scan(&id); err != nil {
			rows.Close()
			return nil, "", err
		}
		ids = append(ids, id)
	}
	rows.Close()
	next := ""
	if len(ids) > limit {
		ids = ids[:limit]
		next = fmt.Sprintf("%d", offset+limit)
	}
	out := make([]Campaign, 0, len(ids))
	for _, id := range ids {
		c, err := db.GetCampaign(id)
		if err != nil {
			return nil, "", err
		}
		out = append(out, *c)
	}
	return out, next, nil
}

// UpdateCampaign edits name/template of a draft (or scheduled) campaign.
func (db *DB) UpdateCampaign(id, name, bodyTemplate string) (*Campaign, error) {
	c, err := db.GetCampaign(id)
	if err != nil {
		return nil, err
	}
	if c.Status != CampaignDraft && c.Status != CampaignScheduled {
		return nil, fmt.Errorf("only draft/scheduled campaigns can be edited")
	}
	if strings.TrimSpace(name) != "" {
		c.Name = strings.TrimSpace(name)
	}
	if strings.TrimSpace(bodyTemplate) != "" {
		c.BodyTemplate = bodyTemplate
	}
	if _, err := db.Exec(`UPDATE campaigns SET name = ?, body_template = ? WHERE id = ?`, c.Name, c.BodyTemplate, id); err != nil {
		return nil, err
	}
	return db.GetCampaign(id)
}

// DeleteCampaign removes a campaign and its recipients (FK cascade).
// Safe in any status: the worker treats a missing campaign as done and
// recipient updates become no-ops.
func (db *DB) DeleteCampaign(id string) error {
	res, err := db.Exec(`DELETE FROM campaigns WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetCampaignStatus transitions with guards. Returns the updated campaign.
func (db *DB) SetCampaignStatus(id, to string) (*Campaign, error) {
	c, err := db.GetCampaign(id)
	if err != nil {
		return nil, err
	}
	allowed := map[string][]string{
		CampaignDraft:     {CampaignSending, CampaignScheduled, CampaignCancelled},
		CampaignScheduled: {CampaignSending, CampaignDraft, CampaignCancelled},
		CampaignSending:   {CampaignPaused, CampaignCancelled, CampaignDone},
		CampaignPaused:    {CampaignSending, CampaignCancelled},
	}
	ok := false
	for _, s := range allowed[c.Status] {
		if s == to {
			ok = true
		}
	}
	if !ok {
		return nil, fmt.Errorf("cannot transition %s -> %s", c.Status, to)
	}
	if _, err := db.Exec(`UPDATE campaigns SET status = ? WHERE id = ?`, to, id); err != nil {
		return nil, err
	}
	return db.GetCampaign(id)
}

// DueScheduled returns scheduled campaigns whose time has come.
func (db *DB) DueScheduled(now time.Time) ([]string, error) {
	rows, err := db.Query(`SELECT id, scheduled_at FROM campaigns WHERE status = ?`, CampaignScheduled)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var id, at string
		if err := rows.Scan(&id, &at); err != nil {
			return nil, err
		}
		t, err := time.Parse(time.RFC3339, at)
		if err != nil || !t.After(now) {
			out = append(out, id)
		}
	}
	return out, rows.Err()
}

// NextSending returns the oldest sending campaign id, if any.
func (db *DB) NextSending() (string, bool) {
	var id string
	if err := db.QueryRow(`SELECT id FROM campaigns WHERE status = ? ORDER BY created_at, id LIMIT 1`, CampaignSending).Scan(&id); err != nil {
		return "", false
	}
	return id, true
}

// ClaimRecipients returns up to n queued recipients with contact details.
func (db *DB) ClaimRecipients(campaignID string, n int) ([]Recipient, error) {
	rows, err := db.Query(`SELECT r.contact_id, c.phone, c.name FROM campaign_recipients r
		JOIN contacts c ON c.id = r.contact_id
		WHERE r.campaign_id = ? AND r.status = ? ORDER BY c.created_at, c.id LIMIT ?`, campaignID, RecQueued, n)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Recipient
	for rows.Next() {
		var r Recipient
		r.CampaignID = campaignID
		r.Status = RecQueued
		if err := rows.Scan(&r.ContactID, &r.Phone, &r.Name); err != nil {
			return nil, err
		}
		out = append(out, r)
	}
	return out, rows.Err()
}

// MarkRecipient updates one delivery state (guarded: only from queued/sending flow).
func (db *DB) MarkRecipient(campaignID, contactID, status, msgID, errMsg string) error {
	now := ""
	if status != RecQueued {
		now = time.Now().UTC().Format(time.RFC3339)
	}
	_, err := db.Exec(`UPDATE campaign_recipients SET status = ?, wa_msg_id = ?, error = ?,
		sent_at = CASE WHEN ? != '' THEN ? ELSE sent_at END
		WHERE campaign_id = ? AND contact_id = ?`, status, msgID, errMsg, now, now, campaignID, contactID)
	return err
}

// MarkByMsgID updates a recipient matched by WhatsApp message id (receipts).
func (db *DB) MarkByMsgID(msgID, status string) (bool, error) {
	if msgID == "" {
		return false, nil
	}
	res, err := db.Exec(`UPDATE campaign_recipients SET status = ? WHERE wa_msg_id = ? AND status IN ('sent','delivered')`, status, msgID)
	if err != nil {
		return false, err
	}
	n, _ := res.RowsAffected()
	return n > 0, nil
}

// RecipientFunnel counts statuses for a campaign.
func (db *DB) RecipientFunnel(campaignID string) (Funnel, error) {
	var f Funnel
	rows, err := db.Query(`SELECT status, COUNT(*) FROM campaign_recipients WHERE campaign_id = ? GROUP BY status`, campaignID)
	if err != nil {
		return f, err
	}
	defer rows.Close()
	for rows.Next() {
		var status string
		var n int
		if err := rows.Scan(&status, &n); err != nil {
			return f, err
		}
		switch status {
		case RecQueued:
			f.Queued = n
		case RecSent:
			f.Sent = n
		case RecDelivered:
			f.Delivered = n
		case RecRead:
			f.Read = n
		case RecReplied:
			f.Replied = n
		case RecFailed:
			f.Failed = n
		}
	}
	return f, rows.Err()
}

// ListRecipients paginates recipients with contact info.
func (db *DB) ListRecipients(campaignID string, limit int, cursor string) ([]Recipient, string, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := 0
	if cursor != "" {
		fmt.Sscanf(cursor, "%d", &offset)
		if offset < 0 {
			offset = 0
		}
	}
	rows, err := db.Query(`SELECT r.contact_id, c.phone, c.name, r.status, r.wa_msg_id, r.error, r.sent_at
		FROM campaign_recipients r JOIN contacts c ON c.id = r.contact_id
		WHERE r.campaign_id = ? ORDER BY c.created_at, c.id LIMIT ? OFFSET ?`, campaignID, limit+1, offset)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []Recipient
	for rows.Next() {
		var r Recipient
		r.CampaignID = campaignID
		if err := rows.Scan(&r.ContactID, &r.Phone, &r.Name, &r.Status, &r.WAMsgID, &r.Error, &r.SentAt); err != nil {
			return nil, "", err
		}
		out = append(out, r)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", offset+limit)
	}
	return out, next, nil
}

// CampaignStatus returns just the status (cheap poll for the worker).
func (db *DB) CampaignStatus(id string) (string, error) {
	var s string
	if err := db.QueryRow(`SELECT status FROM campaigns WHERE id = ?`, id).Scan(&s); err != nil {
		return "", err
	}
	return s, nil
}

// CampaignBody returns template for the worker.
func (db *DB) CampaignBody(id string) (string, error) {
	var b string
	if err := db.QueryRow(`SELECT body_template FROM campaigns WHERE id = ?`, id).Scan(&b); err != nil {
		return "", err
	}
	return b, nil
}
