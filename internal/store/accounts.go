package store

import (
	"database/sql"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

// Account statuses.
const (
	AccountPending      = "pending"
	AccountConnected    = "connected"
	AccountDisconnected = "disconnected"
	AccountLoggedOut    = "logged_out"
)

// Account is one paired WhatsApp number in an org. DeviceJID maps to the
// whatsmeow sqlstore device ("" until first pairing).
type Account struct {
	ID         string `json:"id"`
	OrgID      string `json:"orgId"`
	Label      string `json:"label,omitempty"`
	Phone      string `json:"phone,omitempty"`
	JID        string `json:"jid,omitempty"`
	DeviceJID  string `json:"deviceJid,omitempty"`
	Status     string `json:"status"`
	LastSeenAt string `json:"lastSeenAt,omitempty"`
	CreatedBy  string `json:"createdBy,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

// LegacyAccountID is the implicit account on the SQLite path (no table).
const LegacyAccountID = "legacy"

// CreateAccount inserts a pending account row (pairing happens via Manager).
func (db *DB) CreateAccount(orgID, label, createdBy string) (*Account, error) {
	if !db.IsPostgres() {
		return nil, fmt.Errorf("store: accounts require postgres")
	}
	a := &Account{ID: uuid.NewString(), OrgID: orgID, Label: strings.TrimSpace(label), CreatedBy: createdBy}
	if _, err := db.Exec(`INSERT INTO wa_accounts (id, org_id, label, created_by) VALUES (?, ?, ?, ?)`,
		a.ID, a.OrgID, a.Label, a.CreatedBy); err != nil {
		return nil, err
	}
	return db.GetAccount(a.ID)
}

// GetAccount fetches one account (org-scoped via RLS on PG).
func (db *DB) GetAccount(id string) (*Account, error) {
	a := &Account{}
	if err := db.QueryRow(`SELECT id, org_id, label, phone, jid, device_jid, status, last_seen_at, created_by, created_at
		FROM wa_accounts WHERE id = ?`, id).
		Scan(&a.ID, &a.OrgID, &a.Label, &a.Phone, &a.JID, &a.DeviceJID, &a.Status, &a.LastSeenAt, &a.CreatedBy, &a.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return a, nil
}

// AccountsByOrg lists an org's accounts.
func (db *DB) AccountsByOrg(orgID string) ([]Account, error) {
	if !db.IsPostgres() {
		return nil, nil
	}
	rows, err := db.Query(`SELECT id, org_id, label, phone, jid, device_jid, status, last_seen_at, created_by, created_at
		FROM wa_accounts WHERE org_id = ? ORDER BY created_at, id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.OrgID, &a.Label, &a.Phone, &a.JID, &a.DeviceJID, &a.Status, &a.LastSeenAt, &a.CreatedBy, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// AllAccounts lists every account (owner handle: org enumeration for
// AutoConnect; fail-closed RLS hides all rows from unscoped app handles).
func (db *DB) AllAccounts() ([]Account, error) {
	if !db.IsPostgres() {
		return nil, nil
	}
	rows, err := db.Query(`SELECT id, org_id, label, phone, jid, device_jid, status, last_seen_at, created_by, created_at
		FROM wa_accounts ORDER BY created_at, id`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Account
	for rows.Next() {
		var a Account
		if err := rows.Scan(&a.ID, &a.OrgID, &a.Label, &a.Phone, &a.JID, &a.DeviceJID, &a.Status, &a.LastSeenAt, &a.CreatedBy, &a.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, a)
	}
	return out, rows.Err()
}

// UpdateAccountLabel renames an account.
func (db *DB) UpdateAccountLabel(id, label string) (*Account, error) {
	if _, err := db.GetAccount(id); err != nil {
		return nil, err
	}
	if _, err := db.Exec(`UPDATE wa_accounts SET label = ? WHERE id = ?`, strings.TrimSpace(label), id); err != nil {
		return nil, err
	}
	return db.GetAccount(id)
}

// MarkAccountPaired records a successful pairing (OnPaired callback target).
func (db *DB) MarkAccountPaired(id, phone, jid, deviceJID string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(`UPDATE wa_accounts SET phone = ?, jid = ?, device_jid = ?, status = ?, last_seen_at = ?
		WHERE id = ?`, phone, jid, deviceJID, AccountConnected, now, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// MarkAccountStatus sets status (+last-seen on connected).
func (db *DB) MarkAccountStatus(id, status string) error {
	switch status {
	case AccountPending, AccountConnected, AccountDisconnected, AccountLoggedOut:
	default:
		return fmt.Errorf("invalid account status %q", status)
	}
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE wa_accounts SET status = ?,
		last_seen_at = CASE WHEN ? = 'connected' THEN ? ELSE last_seen_at END
		WHERE id = ?`, status, status, now, id)
	return err
}

// MarkAccountLoggedOut clears pairing state (device wiped).
func (db *DB) MarkAccountLoggedOut(id string) error {
	_, err := db.Exec(`UPDATE wa_accounts SET jid = '', device_jid = '', status = ? WHERE id = ?`, AccountLoggedOut, id)
	return err
}

// DeleteAccount removes an account (grants cascade; campaigns SET NULL).
func (db *DB) DeleteAccount(id string) error {
	res, err := db.Exec(`DELETE FROM wa_accounts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// AccountForCampaign resolves (accountID, deviceJID) for sending.
// Returns ("legacy", "") on SQLite. NULL account on PG (pre-multi-account
// rows) resolves like legacy: the caller falls back to default resolution.
func (db *DB) AccountForCampaign(campaignID string) (accountID, deviceJID string, err error) {
	if !db.IsPostgres() {
		return LegacyAccountID, "", nil
	}
	var aid, djid sql.NullString
	if err := db.QueryRow(`SELECT c.wa_account_id, a.device_jid FROM campaigns c
		LEFT JOIN wa_accounts a ON a.id = c.wa_account_id
		WHERE c.id = ?`, campaignID).Scan(&aid, &djid); err != nil {
		return "", "", err
	}
	if !aid.Valid || aid.String == "" {
		return "", "", nil
	}
	return aid.String, djid.String, nil
}

// ResolveDefaultAccount returns the org's sole account, or "" when the caller
// must choose (zero or many). Callers fall back to it for legacy/unspecified
// sends; multi-account ambiguity is a 400, not a guess.
func ResolveDefaultAccount(accounts []Account) string {
	if len(accounts) == 1 {
		return accounts[0].ID
	}
	return ""
}

// CanAccount reports whether a principal may use an account: admins pass,
// legacy callers without identity (role "") pass, users need a grant
// (member: use+view, viewer: view only). needUse distinguishes
// sending/pairing (member+) from viewing (viewer ok).
func CanAccount(role string, grants map[string]string, accountID string, needUse bool) bool {
	if role == RoleAdmin || role == "" {
		return true
	}
	if !ValidRole(role) {
		return false
	}
	grant, ok := grants[accountID]
	if !ok {
		return false
	}
	if needUse {
		return grant == "member"
	}
	return grant == "member" || grant == "viewer"
}
