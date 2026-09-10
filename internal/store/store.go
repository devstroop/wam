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

// querier is the query surface shared by pools, conns and transactions —
// *sql.DB, *sql.Conn and *sql.Tx all satisfy it natively. DB routes every
// query through it so one method set works pool-backed, request-scoped
// (org transaction) or plain-transactional without signature changes.
type querier interface {
	Exec(query string, args ...any) (sql.Result, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
	Query(query string, args ...any) (*sql.Rows, error)
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	QueryRow(query string, args ...any) *sql.Row
	QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row
}

// DB wraps a SQL handle with WAM helpers. The raw handle stays private so
// every query flows through the rebinding wrappers below. A DB is either
// pool-backed (backend != nil: normal use, migrations, Close) or scoped to
// one request transaction (backend == nil: from BeginOrgTx via middleware).
type DB struct {
	q querier
	// backend is the pool for pool-backed handles, nil for scoped views.
	backend *sql.DB
	// Driver is "sqlite" or "pgx".
	Driver string
	// Path is the SQLite file (empty for Postgres).
	Path string
	// orgID pins the request org on scoped views (""). Inserts stamp it so
	// rows land in the caller's org instead of the 'org_default' fallback.
	orgID string
}

// Tx wraps a transaction with placeholder rebinding. Nested Begin on an
// already-scoped handle reuses the outer transaction (Commit/Rollback
// no-op); only the outermost owner commits or rolls back and releases
// the pinned connection.
type Tx struct {
	tx     *sql.Tx
	q      querier
	conn   *sql.Conn
	pg     bool
	org    string
	nested bool
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
	return db.q.Exec(db.bind(query), args...)
}

func (db *DB) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	return db.q.ExecContext(ctx, db.bind(query), args...)
}

func (db *DB) Query(query string, args ...any) (*sql.Rows, error) {
	return db.q.Query(db.bind(query), args...)
}

func (db *DB) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	return db.q.QueryContext(ctx, db.bind(query), args...)
}

func (db *DB) QueryRow(query string, args ...any) *sql.Row {
	return db.q.QueryRow(db.bind(query), args...)
}

func (db *DB) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	return db.q.QueryRowContext(ctx, db.bind(query), args...)
}

// Pooled reports whether this handle owns a pool (migrations/Close allowed).
func (db *DB) Pooled() bool { return db != nil && db.backend != nil }

// Close releases the underlying pool. Scoped views are closed by their
// owning Tx (Commit/Rollback); calling Close on one is an error.
func (db *DB) Close() error {
	if db.backend == nil {
		return fmt.Errorf("store: close of scoped view")
	}
	return db.backend.Close()
}

// Conn pins one pooled connection (org-scoped work prefers BeginOrgTx).
func (db *DB) Conn(ctx context.Context) (*sql.Conn, error) {
	if db.backend == nil {
		return nil, fmt.Errorf("store: conn of scoped view")
	}
	return db.backend.Conn(ctx)
}

// Begin starts a rebinding transaction. On an already-scoped handle the
// outer transaction is reused (nested Commit/Rollback no-op).
func (db *DB) Begin() (*Tx, error) {
	if q, ok := db.q.(*sql.Tx); ok {
		return &Tx{tx: q, q: q, pg: db.IsPostgres(), nested: true}, nil
	}
	if db.backend == nil {
		return nil, fmt.Errorf("store: begin on closed view")
	}
	tx, err := db.backend.Begin()
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, q: tx, pg: db.IsPostgres()}, nil
}

// BeginTx starts a rebinding transaction bound to ctx.
func (db *DB) BeginTx(ctx context.Context) (*Tx, error) {
	if q, ok := db.q.(*sql.Tx); ok {
		return &Tx{tx: q, q: q, pg: db.IsPostgres(), nested: true}, nil
	}
	if db.backend == nil {
		return nil, fmt.Errorf("store: begin on closed view")
	}
	tx, err := db.backend.BeginTx(ctx, nil)
	if err != nil {
		return nil, err
	}
	return &Tx{tx: tx, q: tx, pg: db.IsPostgres()}, nil
}

// BeginOrgTx pins one pooled connection, opens a transaction with
// SET LOCAL app.org_id applied, and returns it for request scoping.
// The caller must Commit (or Rollback) to release the connection.
// Fail-closed: empty orgID is an error (see WithOrg).
func (db *DB) BeginOrgTx(ctx context.Context, orgID string) (*Tx, error) {
	// Fail closed on Postgres: RLS treats '' as "no scoping". SQLite has no
	// RLS and ignores org context, so any value (including "") works there.
	if db.IsPostgres() && strings.TrimSpace(orgID) == "" {
		return nil, errors.New("store: orgID required")
	}
	if db.backend == nil {
		return nil, fmt.Errorf("store: org tx of scoped view")
	}
	conn, err := db.backend.Conn(ctx)
	if err != nil {
		return nil, err
	}
	tx, err := conn.BeginTx(ctx, nil)
	if err != nil {
		_ = conn.Close()
		return nil, err
	}
	w := &Tx{tx: tx, q: tx, conn: conn, pg: db.IsPostgres(), org: orgID}
	if w.pg {
		if _, err := w.Exec(`SELECT set_config('app.org_id', ?, true)`, orgID); err != nil {
			_ = tx.Rollback()
			_ = conn.Close()
			return nil, err
		}
	}
	return w, nil
}

// DB returns a DB view over this transaction (same method set as pool use),
// carrying the request org for insert stamping.
func (tx *Tx) DB(driver string) *DB {
	return &DB{q: tx.q, Driver: driver, orgID: tx.org}
}

// OrgID reports the request org on scoped views ("" on pool handles).
func (db *DB) OrgID() string { return db.orgID }

// orgOr returns the scoped org, or def on pool handles (legacy single-org).
func (db *DB) orgOr(def string) string {
	if db.orgID != "" {
		return db.orgID
	}
	return def
}

// Commit commits the outermost transaction and releases the pinned
// connection, if any. Nested Commit is a no-op (owner commits).
func (tx *Tx) Commit() error {
	if tx.nested {
		return nil
	}
	err := tx.tx.Commit()
	if tx.conn != nil {
		_ = tx.conn.Close()
	}
	return err
}

// Rollback aborts the outermost transaction and releases the pinned
// connection, if any. Nested Rollback is a no-op (owner rolls back).
func (tx *Tx) Rollback() error {
	if tx.nested {
		return nil
	}
	err := tx.tx.Rollback()
	if tx.conn != nil {
		_ = tx.conn.Close()
	}
	return err
}

func (tx *Tx) Exec(query string, args ...any) (sql.Result, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.q.Exec(query, args...)
}

func (tx *Tx) Query(query string, args ...any) (*sql.Rows, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.q.Query(query, args...)
}

func (tx *Tx) QueryRow(query string, args ...any) *sql.Row {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.q.QueryRow(query, args...)
}

func (tx *Tx) ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.q.ExecContext(ctx, query, args...)
}

func (tx *Tx) QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error) {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.q.QueryContext(ctx, query, args...)
}

func (tx *Tx) QueryRowContext(ctx context.Context, query string, args ...any) *sql.Row {
	if tx.pg {
		query = rebind("pgx", query)
	}
	return tx.q.QueryRowContext(ctx, query, args...)
}

// driverName maps the pg flag back to a Driver string for scoped views.
func (tx *Tx) driverName() string {
	if tx.pg {
		return "pgx"
	}
	return "sqlite"
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
// appURL (least-privilege wam_app, no DDL) so RLS bites. It returns the
// runtime handle plus the owner handle: the worker needs the owner for
// org enumeration (fail-closed RLS hides orgs from the app role) and runs
// all tenant data access inside WithOrg on the runtime handle. On SQLite
// (or owner-only dev mode) both are the same handle.
func OpenAuto(dbPath, ownerURL, appURL string) (app, owner *DB, err error) {
	if strings.TrimSpace(ownerURL) == "" {
		if strings.TrimSpace(appURL) != "" {
			// App-only config (e.g. prod runtime): no DDL, schema must be
			// migrated out-of-band. Queries fail loudly if it isn't.
			app, err = OpenPostgresNoMigrate(appURL)
			return app, app, err
		}
		app, err = OpenSQLite(dbPath)
		return app, app, err
	}
	owner, err = OpenPostgres(ownerURL)
	if err != nil {
		return nil, nil, err
	}
	if strings.TrimSpace(appURL) == "" {
		slog.Warn("store: no app DSN configured; serving via owner DSN (RLS bypassed, dev only)")
		return owner, owner, nil
	}
	app, err = OpenPostgresNoMigrate(appURL)
	if err != nil {
		_ = owner.Close()
		return nil, nil, err
	}
	return app, owner, nil
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
	db := &DB{q: sqldb, backend: sqldb, Driver: "sqlite", Path: path}
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
	if err := db.backend.QueryRow(`SELECT COUNT(*) FROM schema_migrations`).Scan(&n); err != nil {
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
	return &DB{q: sqldb, backend: sqldb, Driver: "pgx"}, nil
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
		if _, err := db.backend.Exec(s); err != nil {
			return fmt.Errorf("store: migrate: %w", err)
		}
	}
	// Best-effort cleanup for DBs created before the approval flow was removed:
	// templates.status is no longer read/written; drop it when present.
	// Old SQLite (<3.35) has no DROP COLUMN — ignore that error.
	_, _ = db.backend.Exec(`ALTER TABLE templates DROP COLUMN status`)
	return nil
}

func (db *DB) migratePostgres() error {
	if _, err := db.backend.Exec(`CREATE TABLE IF NOT EXISTS schema_migrations (version TEXT PRIMARY KEY)`); err != nil {
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
		if err := db.backend.QueryRow(`SELECT COUNT(*) FROM schema_migrations WHERE version = $1`, name).Scan(&applied); err != nil {
			return fmt.Errorf("store: pg migration check %s: %w", name, err)
		}
		if applied > 0 {
			continue
		}
		body, err := migrationFS.ReadFile("migrations/" + name)
		if err != nil {
			return fmt.Errorf("store: pg migration read %s: %w", name, err)
		}
		tx, err := db.backend.Begin()
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
	// Fresh handle racing another writer: create it (idempotent). The app
	// role cannot create orgs — as app, call inside WithOrg(DefaultOrgID)
	// (org context makes the seeded row visible); unscoped use fails here
	// with a permission error, wrapped below.
	if _, ierr := db.Exec(`INSERT INTO orgs (id, name, slug) VALUES (?, ?, ?)`,
		DefaultOrgID, "Default", "default"); ierr != nil {
		if !isUniqueErr(ierr) {
			var pgErr *pgconn.PgError
			if errors.As(ierr, &pgErr) && pgErr.Code == "42501" {
				return nil, fmt.Errorf("store: default org not visible without org context (use WithOrg) or owner handle")
			}
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
// (a bare pool Exec of set_config lands on a random connection). fn receives
// a scoped *DB with the full method set, so all tenant reads/writes inside
// are RLS-isolated. SQLite ignores orgID (no RLS there).
func (db *DB) WithOrg(ctx context.Context, orgID string, fn func(*DB) error) error {
	tx, err := db.BeginOrgTx(ctx, orgID)
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if err := fn(tx.DB(db.Driver)); err != nil {
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
