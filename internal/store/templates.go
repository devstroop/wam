package store

import (
	"database/sql"
	"fmt"
	"strings"

	"github.com/google/uuid"
)

// Template is a reusable message blueprint.
type Template struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Category  string `json:"category"` // marketing | utility | authentication
	Language  string `json:"language"`
	Header    string `json:"header,omitempty"`
	Body      string `json:"body"`
	Footer    string `json:"footer,omitempty"`
	CreatedAt string `json:"createdAt"`
}

// CreateTemplate inserts a new template.
func (db *DB) CreateTemplate(name, body, category, language string) (*Template, error) {
	name = strings.TrimSpace(name)
	if name == "" || len(name) > 100 {
		return nil, fmt.Errorf("invalid template name")
	}
	body = strings.TrimSpace(body)
	if body == "" {
		return nil, fmt.Errorf("body is required")
	}
	if category == "" {
		category = "marketing"
	}
	if language == "" {
		language = "en"
	}
	t := &Template{ID: uuid.NewString(), Name: name, Body: body, Category: category, Language: language}
	if _, err := db.Exec(`INSERT INTO templates (id, name, category, language, body) VALUES (?, ?, ?, ?, ?)`,
		t.ID, t.Name, t.Category, t.Language, t.Body); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("template name already exists")
		}
		return nil, err
	}
	return db.GetTemplate(t.ID)
}

// GetTemplate fetches one.
func (db *DB) GetTemplate(id string) (*Template, error) {
	t := &Template{}
	err := db.QueryRow(`SELECT id, name, category, language, header, body, footer, created_at FROM templates WHERE id = ?`, id).
		Scan(&t.ID, &t.Name, &t.Category, &t.Language, &t.Header, &t.Body, &t.Footer, &t.CreatedAt)
	if err != nil {
		if err == sql.ErrNoRows {
			return nil, sql.ErrNoRows
		}
		return nil, err
	}
	return t, nil
}

// ListTemplates returns all ordered by created_at desc.
func (db *DB) ListTemplates() ([]Template, error) {
	rows, err := db.Query(`SELECT id, name, category, language, header, body, footer, created_at FROM templates ORDER BY created_at DESC, id DESC`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Template
	for rows.Next() {
		var t Template
		if err := rows.Scan(&t.ID, &t.Name, &t.Category, &t.Language, &t.Header, &t.Body, &t.Footer, &t.CreatedAt); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, rows.Err()
}

// UpdateTemplate patches name/body.
func (db *DB) UpdateTemplate(id, name, body string) (*Template, error) {
	cur, err := db.GetTemplate(id)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) != "" {
		cur.Name = strings.TrimSpace(name)
	}
	if strings.TrimSpace(body) != "" {
		cur.Body = strings.TrimSpace(body)
	}
	if _, err := db.Exec(`UPDATE templates SET name = ?, body = ? WHERE id = ?`, cur.Name, cur.Body, id); err != nil {
		if strings.Contains(err.Error(), "UNIQUE") {
			return nil, fmt.Errorf("template name already exists")
		}
		return nil, err
	}
	return db.GetTemplate(id)
}

// DeleteTemplate removes a template.
func (db *DB) DeleteTemplate(id string) error {
	res, err := db.Exec(`DELETE FROM templates WHERE id = ?`, id)
	if err != nil {
		return err
	}
	n, _ := res.RowsAffected()
	if n == 0 {
		return sql.ErrNoRows
	}
	return nil
}

// DuplicateTemplate clones a template with a new name.
func (db *DB) DuplicateTemplate(id string) (*Template, error) {
	src, err := db.GetTemplate(id)
	if err != nil {
		return nil, err
	}
	name := src.Name + " (copy)"
	return db.CreateTemplate(name, src.Body, src.Category, src.Language)
}
