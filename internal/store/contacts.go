package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Contact is one marketing recipient.
type Contact struct {
	ID        string  `json:"id"`
	Phone     string  `json:"phone"`
	Name      string  `json:"name,omitempty"`
	WAJID     string  `json:"waJid,omitempty"`
	WAOk      bool    `json:"waOk,omitempty"`
	Groups    []Group `json:"groups,omitempty"`
	CreatedAt string  `json:"createdAt"`
}

// normalizePhone returns +digits or "" when invalid (7–15 digits).
func normalizePhone(phone string) string {
	var b strings.Builder
	for _, r := range phone {
		if r >= '0' && r <= '9' {
			b.WriteRune(r)
		}
	}
	d := b.String()
	if len(d) < 7 || len(d) > 15 {
		return ""
	}
	return "+" + d
}

func scanContact(row *sql.Row, c *Contact) error {
	var waOk int
	err := row.Scan(&c.ID, &c.Phone, &c.Name, &c.WAJID, &waOk, &c.CreatedAt)
	c.WAOk = waOk == 1
	return err
}

// groupsOf loads groups for one contact.
func (db *DB) groupsOf(contactID string) ([]Group, error) {
	rows, err := db.Query(`SELECT g.id, g.name, g.color, 0 FROM groups g
		JOIN contact_groups cg ON cg.group_id = g.id WHERE cg.contact_id = ? ORDER BY g.name`, contactID)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := []Group{}
	for rows.Next() {
		var g Group
		if err := rows.Scan(&g.ID, &g.Name, &g.Color, &g.ContactCount); err != nil {
			return nil, err
		}
		out = append(out, g)
	}
	return out, rows.Err()
}

// CreateContact inserts a contact and optional memberships.
func (db *DB) CreateContact(phone, name string, groupIDs []string) (*Contact, error) {
	phone = normalizePhone(phone)
	if phone == "" {
		return nil, fmt.Errorf("invalid phone number")
	}
	c := &Contact{ID: uuid.NewString(), Phone: phone, Name: strings.TrimSpace(name)}
	// Stamp the request org on Postgres (scoped views); pool/legacy writes
	// keep the 'org_default' fallback. SQLite has no org_id column.
	var err error
	if db.IsPostgres() {
		_, err = db.Exec(`INSERT INTO contacts (id, phone, name, org_id) VALUES (?, ?, ?, ?)`,
			c.ID, c.Phone, c.Name, db.orgOr(DefaultOrgID))
	} else {
		_, err = db.Exec(`INSERT INTO contacts (id, phone, name) VALUES (?, ?, ?)`, c.ID, c.Phone, c.Name)
	}
	if err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("phone already exists")
		}
		return nil, err
	}
	if len(groupIDs) > 0 {
		if err := db.setMemberships(c.ID, groupIDs); err != nil {
			return nil, err
		}
	}
	return db.GetContact(c.ID)
}

// GetContact fetches one contact with groups.
func (db *DB) GetContact(id string) (*Contact, error) {
	c := &Contact{}
	if err := scanContact(db.QueryRow(`SELECT id, phone, name, wa_jid, wa_ok, created_at FROM contacts WHERE id = ?`, id), c); err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	groups, err := db.groupsOf(id)
	if err != nil {
		return nil, err
	}
	c.Groups = groups
	return c, nil
}

// ListContacts searches by name/phone, filters by group, paginates by offset cursor.
func (db *DB) ListContacts(q, groupID string, limit int, cursor string) ([]Contact, string, error) {
	if limit < 1 || limit > 100 {
		limit = 20
	}
	offset := 0
	if cursor != "" {
		if _, err := fmt.Sscanf(cursor, "%d", &offset); err != nil || offset < 0 {
			offset = 0
		}
	}
	var where []string
	var args []any
	if strings.TrimSpace(q) != "" {
		where = append(where, "(c.name LIKE ? OR c.phone LIKE ?)")
		like := "%" + strings.TrimSpace(q) + "%"
		args = append(args, like, like)
	}
	if groupID != "" {
		where = append(where, "EXISTS (SELECT 1 FROM contact_groups cg WHERE cg.contact_id = c.id AND cg.group_id = ?)")
		args = append(args, groupID)
	}
	w := ""
	if len(where) > 0 {
		w = "WHERE " + strings.Join(where, " AND ")
	}
	query := fmt.Sprintf(`SELECT c.id, c.phone, c.name, c.wa_jid, c.wa_ok, c.created_at FROM contacts c %s
		ORDER BY c.created_at DESC, c.id DESC LIMIT ? OFFSET ?`, w)
	args = append(args, limit+1, offset)
	rows, err := db.Query(query, args...)
	if err != nil {
		return nil, "", err
	}
	defer rows.Close()
	out := []Contact{}
	for rows.Next() {
		var c Contact
		var waOk int
		if err := rows.Scan(&c.ID, &c.Phone, &c.Name, &c.WAJID, &waOk, &c.CreatedAt); err != nil {
			return nil, "", err
		}
		c.WAOk = waOk == 1
		out = append(out, c)
	}
	if err := rows.Err(); err != nil {
		return nil, "", err
	}
	next := ""
	if len(out) > limit {
		out = out[:limit]
		next = fmt.Sprintf("%d", offset+limit)
	}
	for i := range out {
		groups, err := db.groupsOf(out[i].ID)
		if err != nil {
			return nil, "", err
		}
		out[i].Groups = groups
	}
	return out, next, nil
}

// UpdateContact patches phone/name; groupIDs nil = keep, empty = clear.
func (db *DB) UpdateContact(id, phone, name string, groupIDs []string, changeGroups bool) (*Contact, error) {
	cur, err := db.GetContact(id)
	if err != nil {
		return nil, err
	}
	if phone != "" {
		phone = normalizePhone(phone)
		if phone == "" {
			return nil, fmt.Errorf("invalid phone number")
		}
		cur.Phone = phone
	}
	if name != "" {
		cur.Name = strings.TrimSpace(name)
	}
	if _, err := db.Exec(`UPDATE contacts SET phone = ?, name = ? WHERE id = ?`, cur.Phone, cur.Name, id); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("phone already exists")
		}
		return nil, err
	}
	if changeGroups {
		if err := db.setMemberships(id, groupIDs); err != nil {
			return nil, err
		}
	}
	return db.GetContact(id)
}

// DeleteContact removes a contact (memberships + recipient rows cascade).
func (db *DB) DeleteContact(id string) error {
	res, err := db.Exec(`DELETE FROM contacts WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

func (db *DB) setMemberships(contactID string, groupIDs []string) error {
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM contact_groups WHERE contact_id = ?`, contactID); err != nil {
		return err
	}
	for _, gid := range groupIDs {
		if strings.TrimSpace(gid) == "" {
			continue
		}
		if db.IsPostgres() {
			_, err := tx.Exec(`INSERT INTO contact_groups (contact_id, group_id) VALUES (?, ?) ON CONFLICT DO NOTHING`, contactID, gid)
			if err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO contact_groups (contact_id, group_id) VALUES (?, ?)`, contactID, gid); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// ImportContacts bulk-inserts; returns accepted/skipped counts.
func (db *DB) ImportContacts(items []struct {
	Phone string
	Name  string
}) (accepted, skipped int) {
	for _, it := range items {
		phone := normalizePhone(it.Phone)
		if phone == "" {
			skipped++
			continue
		}
		var res sql.Result
		var err error
		if db.IsPostgres() {
			res, err = db.Exec(`INSERT INTO contacts (id, phone, name, org_id) VALUES (?, ?, ?, ?) ON CONFLICT DO NOTHING`,
				uuid.NewString(), phone, strings.TrimSpace(it.Name), db.orgOr(DefaultOrgID))
		} else {
			res, err = db.Exec(`INSERT OR IGNORE INTO contacts (id, phone, name) VALUES (?, ?, ?)`,
				uuid.NewString(), phone, strings.TrimSpace(it.Name))
		}
		if err != nil {
			skipped++
			continue
		}
		if n, _ := res.RowsAffected(); n == 1 {
			accepted++
		} else {
			skipped++ // duplicate phone
		}
	}
	return accepted, skipped
}
