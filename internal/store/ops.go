package store

import (
	"crypto/rand"
	"crypto/sha256"
	"crypto/subtle"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/google/uuid"
)

func opsPG(db *DB) error {
	if !db.IsPostgres() {
		return fmt.Errorf("store: ops require postgres")
	}
	return nil
}

// --- Audit ---

// AuditEntry is one who-did-what row.
type AuditEntry struct {
	ID         string `json:"id"`
	OrgID      string `json:"orgId"`
	ActorID    string `json:"actorId,omitempty"`
	ActorEmail string `json:"actorEmail,omitempty"`
	Action     string `json:"action"`
	Entity     string `json:"entity,omitempty"`
	EntityID   string `json:"entityId,omitempty"`
	Meta       string `json:"meta,omitempty"`
	CreatedAt  string `json:"createdAt"`
}

// Audit appends a trail row (callers ignore errors: auditing never fails
// the request it observes).
func (db *DB) Audit(orgID, actorID, actorEmail, action, entity, entityID, meta string) error {
	if err := opsPG(db); err != nil {
		return err
	}
	if meta == "" {
		meta = "{}"
	}
	_, err := db.Exec(`INSERT INTO audit_logs (id, org_id, actor_id, actor_email, action, entity, entity_id, meta)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), orgID, actorID, actorEmail, action, entity, entityID, meta)
	return err
}

// ListAudit returns newest-first entries (offset cursor like other lists).
func (db *DB) ListAudit(orgID string, limit int, cursor string) ([]AuditEntry, string, error) {
	if err := opsPG(db); err != nil {
		return nil, "", err
	}
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
	rows, err := db.Query(`SELECT id, org_id, actor_id, actor_email, action, entity, entity_id, meta, created_at
		FROM audit_logs WHERE org_id = ? ORDER BY created_at DESC, id DESC LIMIT ? OFFSET ?`,
		orgID, limit+1, offset)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	var out []AuditEntry
	for rows.Next() {
		var e AuditEntry
		if err := rows.Scan(&e.ID, &e.OrgID, &e.ActorID, &e.ActorEmail, &e.Action,
			&e.Entity, &e.EntityID, &e.Meta, &e.CreatedAt); err != nil {
			return nil, "", err
		}
		out = append(out, e)
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

// --- API keys ---

// APIKey is a Bearer credential (hash never leaves the store).
type APIKey struct {
	ID        string   `json:"id"`
	OrgID     string   `json:"orgId"`
	UserID    string   `json:"userId"`
	Prefix    string   `json:"prefix"`
	Name      string   `json:"name,omitempty"`
	Scopes    []string `json:"scopes"`
	ExpiresAt string   `json:"expiresAt,omitempty"`
	CreatedAt string   `json:"createdAt"`
}

// keyPrefixLen exposes enough of the raw key for indexed lookup.
const keyPrefixLen = 8

// CreateAPIKey mints a key, returning the row + raw secret (shown once).
func (db *DB) CreateAPIKey(orgID, userID, name string, scopes []string, expiresAt time.Time) (*APIKey, string, error) {
	if err := opsPG(db); err != nil {
		return nil, "", err
	}
	b := make([]byte, 24)
	if _, err := rand.Read(b); err != nil {
		return nil, "", err
	}
	raw := "wam_" + base64.RawURLEncoding.EncodeToString(b)
	prefix := raw[4 : 4+keyPrefixLen]
	sum := sha256.Sum256([]byte(raw))
	hash := fmt.Sprintf("%x", sum)
	exp := ""
	if !expiresAt.IsZero() {
		exp = expiresAt.UTC().Format(time.RFC3339)
	}
	scopesJSON, _ := json.Marshal(scopes)
	k := &APIKey{ID: uuid.NewString(), OrgID: orgID, UserID: userID, Prefix: prefix, Name: strings.TrimSpace(name)}
	if _, err := db.Exec(`INSERT INTO api_keys (id, org_id, user_id, prefix, key_hash, name, scopes, expires_at)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		k.ID, k.OrgID, k.UserID, k.Prefix, hash, k.Name, string(scopesJSON), exp); err != nil {
		return nil, "", err
	}
	row, err := db.GetAPIKey(k.ID, orgID)
	if err != nil {
		return nil, "", err
	}
	return row, raw, nil
}

// GetAPIKey fetches one key (org-scoped, no secret).
func (db *DB) GetAPIKey(id, orgID string) (*APIKey, error) {
	k := &APIKey{}
	var scopes, exp, revoked string
	err := db.QueryRow(`SELECT id, org_id, user_id, prefix, name, scopes, expires_at, revoked_at, created_at
		FROM api_keys WHERE id = ? AND org_id = ?`, id, orgID).
		Scan(&k.ID, &k.OrgID, &k.UserID, &k.Prefix, &k.Name, &scopes, &exp, &revoked, &k.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(scopes), &k.Scopes)
	k.ExpiresAt = exp
	return k, nil
}

// LookupKey authenticates a raw Bearer token: prefix lookup + constant-time
// hash compare + revocation/expiry. Returns key with scopes.
func (db *DB) LookupKey(raw string) (*APIKey, error) {
	if !strings.HasPrefix(raw, "wam_") || len(raw) < 4+keyPrefixLen {
		return nil, sql.ErrNoRows
	}
	prefix := raw[4 : 4+keyPrefixLen]
	rows, err := db.Query(`SELECT id, org_id, user_id, prefix, key_hash, name, scopes, expires_at, revoked_at, created_at
		FROM api_keys WHERE prefix = ?`, prefix)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	sum := sha256.Sum256([]byte(raw))
	want := fmt.Sprintf("%x", sum)
	for rows.Next() {
		var k APIKey
		var hash, scopes, exp, revoked string
		if err := rows.Scan(&k.ID, &k.OrgID, &k.UserID, &k.Prefix, &hash, &k.Name,
			&scopes, &exp, &revoked, &k.CreatedAt); err != nil {
			return nil, err
		}
		if subtle.ConstantTimeCompare([]byte(hash), []byte(want)) != 1 {
			continue
		}
		if strings.TrimSpace(revoked) != "" {
			return nil, sql.ErrNoRows
		}
		if exp != "" {
			if t, err := time.Parse(time.RFC3339, exp); err != nil || time.Now().After(t) {
				return nil, sql.ErrNoRows
			}
		}
		_ = json.Unmarshal([]byte(scopes), &k.Scopes)
		k.ExpiresAt = exp
		return &k, nil
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return nil, sql.ErrNoRows
}

// ListAPIKeys returns an org's keys newest-first (no secrets).
func (db *DB) ListAPIKeys(orgID string) ([]APIKey, error) {
	if err := opsPG(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, org_id, user_id, prefix, name, scopes, expires_at, revoked_at, created_at
		FROM api_keys WHERE org_id = ? AND revoked_at = '' ORDER BY created_at DESC, id DESC`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []APIKey
	for rows.Next() {
		var k APIKey
		var scopes, exp, revoked string
		if err := rows.Scan(&k.ID, &k.OrgID, &k.UserID, &k.Prefix, &k.Name,
			&scopes, &exp, &revoked, &k.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(scopes), &k.Scopes)
		k.ExpiresAt = exp
		out = append(out, k)
	}
	return out, rows.Err()
}

// RevokeAPIKey kills a key (row kept for audit).
func (db *DB) RevokeAPIKey(id, orgID string) error {
	if err := opsPG(db); err != nil {
		return err
	}
	now := time.Now().UTC().Format(time.RFC3339)
	res, err := db.Exec(`UPDATE api_keys SET revoked_at = ? WHERE id = ? AND org_id = ? AND revoked_at = ''`,
		now, id, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// TouchKeyUsed records last use (best-effort, callers ignore errors).
func (db *DB) TouchKeyUsed(id string) error {
	now := time.Now().UTC().Format(time.RFC3339)
	_, err := db.Exec(`UPDATE api_keys SET last_used_at = ? WHERE id = ?`, now, id)
	return err
}

// --- Outbound webhooks ---

// WebhookEvents enumerates subscribable topics.
var WebhookEvents = []string{
	"message.sent", "message.delivered", "message.read", "message.failed",
	"campaign.done",
}

// WebhookEndpoint is an org-owned outbound target (secret server-side only).
type WebhookEndpoint struct {
	ID        string   `json:"id"`
	OrgID     string   `json:"orgId"`
	URL       string   `json:"url"`
	Events    []string `json:"events"`
	CreatedBy string   `json:"createdBy,omitempty"`
	CreatedAt string   `json:"createdAt"`
}

// CreateEndpoint registers a target (https URLs only).
func (db *DB) CreateEndpoint(orgID, url, secret string, events []string, by string) (*WebhookEndpoint, error) {
	if err := opsPG(db); err != nil {
		return nil, err
	}
	url = strings.TrimSpace(url)
	if !strings.HasPrefix(url, "https://") {
		return nil, fmt.Errorf("invalid endpoint url (https required)")
	}
	allowed := map[string]bool{}
	for _, e := range WebhookEvents {
		allowed[e] = true
	}
	for _, e := range events {
		if !allowed[e] {
			return nil, fmt.Errorf("invalid event %q", e)
		}
	}
	eventsJSON, _ := json.Marshal(events)
	e := &WebhookEndpoint{ID: uuid.NewString(), OrgID: orgID, URL: url, Events: events, CreatedBy: by}
	if _, err := db.Exec(`INSERT INTO webhook_endpoints (id, org_id, url, secret, events, created_by)
		VALUES (?, ?, ?, ?, ?, ?)`, e.ID, e.OrgID, e.URL, secret, string(eventsJSON), e.CreatedBy); err != nil {
		return nil, err
	}
	return db.GetEndpoint(e.ID, orgID)
}

// GetEndpoint fetches one target (no secret).
func (db *DB) GetEndpoint(id, orgID string) (*WebhookEndpoint, error) {
	e := &WebhookEndpoint{}
	var events string
	if err := db.QueryRow(`SELECT id, org_id, url, events, created_by, created_at
		FROM webhook_endpoints WHERE id = ? AND org_id = ?`, id, orgID).
		Scan(&e.ID, &e.OrgID, &e.URL, &events, &e.CreatedBy, &e.CreatedAt); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	_ = json.Unmarshal([]byte(events), &e.Events)
	return e, nil
}

// ListEndpoints returns org targets (no secrets).
func (db *DB) ListEndpoints(orgID string) ([]WebhookEndpoint, error) {
	if err := opsPG(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, org_id, url, events, created_by, created_at
		FROM webhook_endpoints WHERE org_id = ? ORDER BY created_at, id`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []WebhookEndpoint
	for rows.Next() {
		var e WebhookEndpoint
		var events string
		if err := rows.Scan(&e.ID, &e.OrgID, &e.URL, &events, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		_ = json.Unmarshal([]byte(events), &e.Events)
		out = append(out, e)
	}
	return out, rows.Err()
}

// DeleteEndpoint removes a target.
func (db *DB) DeleteEndpoint(id, orgID string) error {
	if err := opsPG(db); err != nil {
		return err
	}
	res, err := db.Exec(`DELETE FROM webhook_endpoints WHERE id = ? AND org_id = ?`, id, orgID)
	if err != nil {
		return err
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// EndpointWithSecret carries the signing secret (delivery only, never API).
type EndpointWithSecret struct {
	WebhookEndpoint
	Secret string
}

// EndpointsForEvent returns subscribed targets WITH secrets.
func (db *DB) EndpointsForEvent(orgID, event string) ([]EndpointWithSecret, error) {
	if err := opsPG(db); err != nil {
		return nil, err
	}
	rows, err := db.Query(`SELECT id, org_id, url, secret, events, created_by, created_at
		FROM webhook_endpoints WHERE org_id = ?`, orgID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []EndpointWithSecret
	for rows.Next() {
		var e EndpointWithSecret
		var events string
		if err := rows.Scan(&e.ID, &e.OrgID, &e.URL, &e.Secret, &events, &e.CreatedBy, &e.CreatedAt); err != nil {
			return nil, err
		}
		var evts []string
		_ = json.Unmarshal([]byte(events), &evts)
		e.Events = evts
		for _, ev := range evts {
			if ev == event {
				out = append(out, e)
				break
			}
		}
	}
	return out, rows.Err()
}

// RecordDelivery logs one attempt outcome.
func (db *DB) RecordDelivery(orgID, endpointID, event, payload, status string, attempts int, lastErr string) error {
	if err := opsPG(db); err != nil {
		return err
	}
	_, err := db.Exec(`INSERT INTO webhook_deliveries (id, org_id, endpoint_id, event, payload, status, attempts, last_error)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		uuid.NewString(), orgID, endpointID, event, payload, status, attempts, lastErr)
	return err
}
