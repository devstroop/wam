package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Group is a label/segment for contacts.
type Group struct {
	ID           string `json:"id"`
	Name         string `json:"name"`
	Color        string `json:"color,omitempty"`
	ContactCount int    `json:"contactCount"`
}

// CreateGroup inserts a label.
func (db *DB) CreateGroup(name, color string) (*Group, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, fmt.Errorf("invalid group name")
	}
	g := &Group{ID: uuid.NewString(), Name: name, Color: strings.TrimSpace(color)}
	if _, err := db.Exec(`INSERT INTO groups (id, name, color) VALUES (?, ?, ?)`, g.ID, g.Name, g.Color); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("group name already exists")
		}
		return nil, err
	}
	return db.GetGroup(g.ID)
}

// GetGroup fetches one group with member count.
func (db *DB) GetGroup(id string) (*Group, error) {
	g := &Group{}
	err := db.QueryRow(`SELECT g.id, g.name, g.color,
		(SELECT COUNT(*) FROM contact_groups cg WHERE cg.group_id = g.id)
		FROM groups g WHERE g.id = ?`, id).Scan(&g.ID, &g.Name, &g.Color, &g.ContactCount)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return g, nil
}

// ListGroups returns all labels with counts, ordered by name.
func (db *DB) ListGroups() ([]Group, error) {
	rows, err := db.Query(`SELECT g.id, g.name, g.color,
		(SELECT COUNT(*) FROM contact_groups cg WHERE cg.group_id = g.id)
		FROM groups g ORDER BY g.name`)
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

// UpdateGroup renames/recolors. Empty name keeps current.
func (db *DB) UpdateGroup(id, name, color string) (*Group, error) {
	cur, err := db.GetGroup(id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) != "" {
		cur.Name = strings.TrimSpace(name)
	}
	cur.Color = strings.TrimSpace(color)
	if _, err := db.Exec(`UPDATE groups SET name = ?, color = ? WHERE id = ?`, cur.Name, cur.Color, id); err != nil {
		if isUniqueErr(err) {
			return nil, fmt.Errorf("group name already exists")
		}
		return nil, err
	}
	return db.GetGroup(id)
}

// DeleteGroup removes a label (memberships cascade).
func (db *DB) DeleteGroup(id string) error {
	res, err := db.Exec(`DELETE FROM groups WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// SetGroupMembers replaces membership (ignores unknown contact ids).
func (db *DB) SetGroupMembers(groupID string, contactIDs []string) error {
	if _, err := db.GetGroup(groupID); err != nil {
		return err
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM contact_groups WHERE group_id = ?`, groupID); err != nil {
		return err
	}
	for _, cid := range contactIDs {
		if strings.TrimSpace(cid) == "" {
			continue
		}
		if db.IsPostgres() {
			_, err := tx.Exec(`INSERT INTO contact_groups (contact_id, group_id)
				SELECT ?, ? WHERE EXISTS (SELECT 1 FROM contacts WHERE id = ?) ON CONFLICT DO NOTHING`, cid, groupID, cid)
			if err != nil {
				return err
			}
			continue
		}
		if _, err := tx.Exec(`INSERT OR IGNORE INTO contact_groups (contact_id, group_id)
			SELECT ?, ? WHERE EXISTS (SELECT 1 FROM contacts WHERE id = ?)`, cid, groupID, cid); err != nil {
			return err
		}
	}
	return tx.Commit()
}
