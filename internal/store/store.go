// Package store owns persistence for app tables.
//
// SaaS platform branch: dual-engine. SQLite (modernc) remains the legacy
// local path; Postgres (pgx stdlib) is authoritative when WAM_DATABASE_URL
// is set. Postgres adds orgs + org_id + FORCE RLS tenant isolation via
// versioned SQL migrations in migrations/*.sql. SQLite keeps the original
// code-first IF NOT EXISTS path untouched.
//
// Query convention: write SQLite-style "?" placeholders everywhere; DB
// methods rebind to $n automatically on Postgres. The only dialect forks
// are INSERT OR IGNORE (→ ON CONFLICT DO NOTHING) and strftime cutoffs,
// handled via IsPostgres()/isUniqueErr()/cutoff helpers.
package store

import (
	"context"
	"database/sql"
	"embed"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/jackc/pgx/v5/pgconn"
	_ "github.com/jackc/pgx/v5/stdlib"
	_ "modernc.org/sqlite"
)

//go:embed migrations/*.sql
var migrationFS embed.FS

// DefaultOrgID is the pre-UMS single-tenant org. All legacy rows belong here.
const DefaultOrgID = "org_default"

// DB wraps a SQL handle with WAM helpers. The raw handle stays private so
// every query flows through the rebinding wrappers below.
type DB struct {
	sql *sql.DB
	// Driver is "sqlite" or "pgx".
	Driver string
	// Path is the SQLite file (empty for Postgres).
	Path string
}

// Tx wraps *sql.Tx with placeholder rebinding.
type Tx struct {
	*sql.Tx
	pg bool
}

// IsPostgres reports whether this handle talks to Postgres.
func (db *DB) IsPostgres() bool { return db != nil && db.Driver == "pgx" }

// rebind converts ? placeholders to $n for Postgres; no-op for SQLite.
// Convention: never put a literal ? inside a query string (e.g. in JSON
// operators) — it would be rewritten. None exist today; keep it that way.
func rebind(driver, q string) string {
	if driver != "pgx" || !strings.Contains(q, "?") {
		return q
	}
	var b strings.Builder
	n := 0
	for i := 0; i < len(q); i++ {
		if q[i] == '?' {
			n++
			fmt.Fprintf(&b, "$%d", n)
		} else {
			b.WriteByte(q[i])
		}
	}
	return b.String()
}

func (db *DB) bind(q string) string { return rebind(db.Driver, q) }

// Exec/Query/QueryRow wrap the private handle with placeholder rebinding.
func (db *DB) Exec(query string, args ...any) (sql.Result, error) {
	return db.sql.Exec(db.bind(query), args...)
}

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.sql.ExecContext(ctx, db.bind(query), args...)
}

func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.sql.Query(db.bind(query), args...)
}

func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.sql.QueryContext(ctx, db.bind(query), args...)
}

func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	return db.sql.QueryRow(db.bind(query), args...)
}

func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.sql.QueryRowContext(ctx, db.bind(query), args...)
}

// Close releases the underlying pool.
func (db *DB) Close() error { return db.sql.Close() }

// Conn pins one pooled connection (for org-scoped work; see WithOrg).
func (db *DB) Conn(ctx context.Context) (*sql.Conn, error) { return db.sql.Conn(ctx) }

// Begin starts a rebinding transaction.
func (db *DB) Begin() (*Tx, error) {
	tx, err := db.sql.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{Tx: tx, pg: db.IsPostgres()}, nil
}

func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.Tx.Exec(query, args...)
}

func (tx *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.Tx.Query(query, args...)
}

func (tx *Tx) QueryRow(query string, args ...any) *sql.Row {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.Tx.QueryRow(query, args...)
}

func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.Tx.ExecContext(ctx, query, args...)
}

func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.Tx.QueryContext(ctx, query, args...)
}

func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.Tx.QueryRowContext(ctx, query, args...)
}

// isUniqueErr matches unique-violation errors on both engines: Postgres via
// the wire-protocol code (no substring guessing), SQLite via message text.
func isUniqueErr(err error) bool {
	if err == nil {
		return false
	}
	var pgErr *pgconn.PgError
	if errors.As(err, &pgErr) {
		return pgErr.Code == "23505"
	}
	s := err.Error()
	return strings.Contains(s, "UNIQUE") || strings.Contains(s, "unique")
}

// Open keeps the legacy SQLite signature (used by local dev/tests).
func Open(path string) (*DB, error) { return OpenSQLite(path) }

// OpenAuto picks Postgres when ownerURL is set, else SQLite.
// Postgres boots in two roles: migrate with the owner DSN, then serve via
// appURL (least-privilege wam_app, no DDL) so RLS bites. Empty appURL falls
// back to serving via the owner DSN with a warning (local dev only —
// superusers bypass RLS, so there is no tenant isolation in that mode).
func OpenAuto(dbPath, ownerURL, appURL string) (*DB, error) {
	if strings.TrimSpace(ownerURL) == "" {
		if strings.TrimSpace(appURL) != "" {
			// App-only config (e.g. prod runtime): no DDL, schema must be
			// migrated out-of-band. Queries fail loudly if it isn't.
			return OpenPostgresNoMigrate(appURL)
		}
		return OpenSQLite(dbPath)
	}
	owner, err := OpenPostgres(ownerURL)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(appURL) == "" {
		slog.Warn("store: no app DSN configured; serving via owner DSN (RLS bypassed, dev only)")
		return owner, nil
	}
	_ = owner.Close()
	return OpenPostgresNoMigrate(appURL)
}

// OpenSQLite creates parent dirs, opens SQLite with sane pragmas, runs migrations.
func OpenSQLite(path string) (*DB, error) {
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
	db := &DB{sql: sqldb, Driver: "sqlite", Path: path}
	if err := db.migrateSQLite(); err != nil {
		_ = sqldb.Close()
		return nil, err
	}
	return db, nil
}

// OpenPostgres connects to Postgres and runs versioned SQL migrations.
func OpenPostgres(dsn string) (*DB, error) {
	db, err := openPostgres(dsn)
	if err != nil {
		return nil, err
	}
	if err := db.migratePostgres(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return db, nil
}

// OpenPostgresNoMigrate connects without running DDL migrations — for the
// least-privilege runtime role (wam_app), which lacks CREATE privileges.
// Schema must be migrated out-of-band (wam migrate-schema with owner DSN).
// Fails fast when the schema is missing instead of 500ing on first query.
func OpenPostgresNoMigrate(dsn string) (*DB, error) {
	db, err := openPostgres(dsn)
	if err != nil {
		return nil, err
	}
	var n int
	if err := db.sql.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
		_ = db.Close()
		return nil, fmt.Errorf("store: schema not migrated (run wam migrate-schema with the owner DSN): %w", err)
	}
	return db, nil
}

func openPostgres(dsn string) (*DB, error) {
	sqldb, err := sql.Open("pgx", dsn)
	if err != nil {
		return nil, fmt.Errorf("store: pg open: %w", err)
	}
	sqldb.SetMaxOpenConns(25)
	sqldb.SetMaxIdleConns(5)
	if err := sqldb.Ping(); err != nil {
		_ = sqldb.Close()
		return nil, fmt.Errorf("store: pg ping: %w", err)
	}
	return &DB{sql: sqldb, Driver: "pgx"}, nil
}

func (db *DB) migrateSQLite() error {
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
		if _, err := db.sql.Exec(s); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	// Best-effort cleanup for DBs created before the approval flow was removed:
	// templates.status is no longer read/written; drop it when present.
	// Old SQLite (<3.35) has no DROP COLUMN — ignore that error.
	_, _ = db.sql.Exec(`ALTER TABLE templates DROP COLUMN status`)
	return nil
}

func (db *DB) migratePostgres() error {
	if _, err := db.sql.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
		return fmt.Errorf("store: pg migrations table: %w", err)
	}
	entries, err := migrationFS.ReadDir("migrations")
	if err != nil {
		return fmt.Errorf("store: pg migrations read: %w", err)
	}
	var names []string
	for _, e := range entries {
		if !e.IsDir() && strings.HasSuffix(e.Name(), ".sql") {
			names = append(names, e.Name())
		}
	}
	sort.Strings(names)
	for _, name := range names {
		var applied int
		if err := db.sql.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, name).Scan(&applied); err != nil {
			return fmt.Errorf("store: pg migration check %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: pg migration read %s: %w", name, err)
		}
		tx, err := db.sql.Begin()
		if err != nil {
			return err
		}
		// Serialize concurrent boots: the check-then-insert above races when
		// two servers start at once; the xact-scoped lock makes the loser wait
		// and then see the winner's schema_migrations row.
		if _, err := tx.Exec(`SELECT pg_advisory_xact_lock(hashtext('wam_migrations'))`); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: pg migration lock %s: %w", name, err)
		}
		if _, err := tx.Exec(string(body)); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: pg migration %s: %w", name, err)
		}
		if _, err := tx.Exec(`INSERT INTO schema_migrations (version) VALUES ($1)`, name); err != nil {
			_ = tx.Rollback()
			return fmt.Errorf("store: pg migration record %s: %w", name, err)
		}
		if err := tx.Commit(); err != nil {
			return fmt.Errorf("store: pg migration commit %s: %w", name, err)
		}
	}
	return nil
}

// --- Tenant helpers (RLS) ---

// Org is a SaaS tenant.
type Org struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Slug      string `json:"slug"`
	CreatedAt string `json:"createdAt"`
}

// DefaultOrg returns the pre-UMS single-tenant org, creating it if needed.
// Postgres-only: the SQLite schema has no orgs table.
func (db *DB) DefaultOrg() (*Org, error) {
	if !db.IsPostgres() {
		return nil, fmt.Errorf("store: orgs require postgres")
	}
	o := &Org{}
	err := db.QueryRow(`SELECT id, name, slug, created_at FROM orgs WHERE id = ?`, DefaultOrgID).
		Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedAt)
	if err == nil {
		return o, nil
	}
	if err != sql.ErrNoRows {
		return nil, err
	}
	// Fresh handle racing another writer: create it (idempotent).
	if _, ierr := db.Exec(`INSERT INTO orgs (id, name, slug) VALUES (?, ?, ?)`,
		DefaultOrgID, "Default", "default"); ierr != nil {
		if !isUniqueErr(ierr) {
			return nil, ierr
		}
	}
	if err := db.QueryRow(`SELECT id, name, slug, created_at FROM orgs WHERE id = ?`, DefaultOrgID).
		Scan(&o.ID, &o.Name, &o.Slug, &o.CreatedAt); err != nil {
		return nil, err
	}
	return o, nil
}

// WithOrg runs fn inside a transaction pinned to a single pooled connection
// with SET LOCAL app.org_id applied — the only pool-safe org-scoping pattern
// (a bare pool Exec of set_config lands on a random connection). This is the
// primitive feat/ums request middleware will build on. SQLite ignores orgID.
func (db *DB) WithOrg(ctx context.Context, orgID string, fn func(*Tx) error) error {
	// Fail closed: the RLS policies treat ''/NULL as "no scoping", so an
	// empty orgID here would silently expose all tenants.
	if strings.TrimSpace(orgID) == "" {
		return errors.New("store: orgID required")
	}
	conn, err := db.sql.Conn(ctx)
	if err != nil {
		return err
	}
	defer conn.Close()
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	wtx := &Tx{Tx: tx, pg: db.IsPostgres()}
	if db.IsPostgres() {
		if _, err := wtx.Exec(`SELECT set_config('app.org_id', ?, true)`, orgID); err != nil {
			return err
		}
	}
	if err := fn(wtx); err != nil {
		return err
	}
	return tx.Commit()
}

// Stats returns headline counts for the dashboard (zeros until data exists).
func (db *DB) Stats() (contacts, groups, campaigns, queued int) {
	_ = db.QueryRow(`SELECT COUNT(*) FROM contacts`).Scan(&contacts)
	_ = db.QueryRow(`SELECT COUNT(*) FROM groups`).Scan(&groups)
	_ = db.QueryRow(`SELECT COUNT(*) FROM campaigns`).Scan(&campaigns)
	_ = db.QueryRow(`SELECT COUNT(*) FROM campaign_recipients WHERE status='queued'`).Scan(&queued)
	return contacts, groups, campaigns, queued
}
