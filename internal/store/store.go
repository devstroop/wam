// Package store owns SQLite persistence (app tables). The same SQLite file
// will host the whatsmeow sqlstore container (shared DB file,
// separate table prefixes — whatsmeow uses its own schema).
//
// Schema:
//
//	contacts, groups, contact_groups, campaigns, campaign_recipients, templates.
//
// Migrations are code-first IF NOT EXISTS, mirroring notalk's approach.
package store

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"

	_ "modernc.org/sqlite"
)

// DB wraps *sql.DB with WAM helpers.
type DB struct {
	*sql.DB
	Path string
}

// Open creates parent dirs, opens SQLite with sane pragmas, runs migrations.
func Open(path string) (*DB, error) {
	if path == "" {
		path = "./data/wam.db"
	}
	if dir := filepath.Dir(path); dir != "" && dir != "." {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			return nil, fmt.Errorf("store: mkdir %s: %w", dir, err)
		}
	}
	dsn := fmt.Sprintf("file:%s?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=foreign_keys(ON)", path)
	sqldb, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: open: %w", err)
	}
	sqldb.SetMaxOpenConns(1)
	if err := sqldb.Ping(); err != nil {
		return nil, fmt.Errorf("store: ping: %w", err)
	}
	db := &DB{DB: sqldb, Path: path}
	if err := db.migrate(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

func (db *DB) migrate() error {
	stmts := []string{
		`CREATE TABLE IF NOT EXISTS contacts (
			id TEXT PRIMARY KEY,
			phone TEXT NOT NULL UNIQUE,
			name TEXT NOT NULL DEFAULT '',
			wa_jid TEXT NOT NULL DEFAULT '',
			wa_ok INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contacts_phone ON contacts(phone)`,
		`CREATE TABLE IF NOT EXISTS groups (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			color TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE TABLE IF NOT EXISTS contact_groups (
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
			PRIMARY KEY (contact_id, group_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_contact_groups_group ON contact_groups(group_id)`,
		`CREATE TABLE IF NOT EXISTS campaigns (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL,
			status TEXT NOT NULL DEFAULT 'draft',
			body_template TEXT NOT NULL DEFAULT '',
			audience_filter TEXT NOT NULL DEFAULT '{}',
			scheduled_at TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
		`CREATE INDEX IF NOT EXISTS idx_campaigns_status ON campaigns(status)`,
		`CREATE TABLE IF NOT EXISTS campaign_recipients (
			campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
			contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
			status TEXT NOT NULL DEFAULT 'queued',
			wa_msg_id TEXT NOT NULL DEFAULT '',
			error TEXT NOT NULL DEFAULT '',
			sent_at TEXT NOT NULL DEFAULT '',
			PRIMARY KEY (campaign_id, contact_id)
		)`,
		`CREATE INDEX IF NOT EXISTS idx_recipients_status ON campaign_recipients(campaign_id, status)`,
		`CREATE TABLE IF NOT EXISTS templates (
			id TEXT PRIMARY KEY,
			name TEXT NOT NULL UNIQUE,
			category TEXT NOT NULL DEFAULT 'marketing',
			language TEXT NOT NULL DEFAULT 'en',
			header TEXT NOT NULL DEFAULT '',
			body TEXT NOT NULL,
			footer TEXT NOT NULL DEFAULT '',
			created_at TEXT NOT NULL DEFAULT (strftime('%Y-%m-%dT%H:%M:%fZ','now'))
		)`,
	}
	for _, s := range stmts {
		if _, err := db.Exec(s); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	// Best-effort cleanup for DBs created before the approval flow was removed:
	// templates.status is no longer read/written; drop it when present.
	// Old SQLite (<3.35) has no DROP COLUMN — ignore that error.
	_, _ = db.Exec(`ALTER TABLE templates DROP COLUMN status`)
	return nil
}

// Stats returns headline counts for the dashboard (zeros until data exists).
func (db *DB) Stats() (contacts, groups, campaigns, queued int) {
	_ = db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&contacts)
	_ = db.QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&groups)
	_ = db.QueryRow(`SELECT COUNT(*) FROM campaigns`).Scan(&campaigns)
	_ = db.QueryRow(`SELECT COUNT(*) FROM campaign_recipients WHERE status='queued'`).Scan(&queued)
	return contacts, groups, campaigns, queued
}
