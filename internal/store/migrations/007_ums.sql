-- 007_ums: identity, RBAC and per-account grants.
--
-- Tenancy notes:
--   * users/sessions/invites/resets/verifications carry NO RLS. They are
--     authN infrastructure looked up by email or unguessable token BEFORE any
--     org context exists (login/verify/accept flows); every query filters by
--     session user or token hash server-side.
--   * memberships/grants carry org_id with explicit predicates in code
--     (always filtered by session user/org); RLS stays on the tenant DATA
--     tables below, which flip from permissive bridge to fail-closed here:
--     every data read/write now requires SET LOCAL app.org_id (WithOrg /
--     request middleware). Anything that forgets org context sees nothing.
--
-- Timestamps stay TEXT (RFC3339 UTC) per the repo convention.

CREATE TABLE IF NOT EXISTS users (
	id TEXT PRIMARY KEY,
	email TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	password_hash TEXT NOT NULL DEFAULT '',
	email_verified_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_users_email ON users(email);

-- Roles are data (extensible to custom roles later); the Go permission map in
-- store/ums.go mirrors these seeds so per-request checks avoid a join.
CREATE TABLE IF NOT EXISTS roles (
	code TEXT PRIMARY KEY,
	permissions TEXT NOT NULL DEFAULT '[]'
);
INSERT INTO roles (code, permissions) VALUES
	('admin', '["*"]'),
	('user', '["contacts:manage","templates:manage","campaigns:manage","campaigns:send","analytics:view","accounts:view","accounts:use"]')
	ON CONFLICT (code) DO NOTHING;

CREATE TABLE IF NOT EXISTS memberships (
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	role TEXT NOT NULL DEFAULT 'user',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
	PRIMARY KEY (user_id, org_id)
);
CREATE INDEX IF NOT EXISTS idx_memberships_org ON memberships(org_id);
CREATE INDEX IF NOT EXISTS idx_memberships_user ON memberships(user_id);

-- Per-account grants for non-admins (admin bypasses: implicit all accounts).
-- account_id has no FK yet — wa_accounts lands in feat/multi-account, which
-- adds the constraint. org_id is denormalized for explicit scoping.
CREATE TABLE IF NOT EXISTS wa_account_grants (
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	account_id TEXT NOT NULL,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	role TEXT NOT NULL DEFAULT 'member',
	granted_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
	PRIMARY KEY (user_id, account_id)
);
CREATE INDEX IF NOT EXISTS idx_grants_org ON wa_account_grants(org_id);

CREATE TABLE IF NOT EXISTS invites (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	org_name TEXT NOT NULL DEFAULT '',
	email TEXT NOT NULL,
	role TEXT NOT NULL DEFAULT 'user',
	token_hash TEXT NOT NULL UNIQUE,
	invited_by TEXT NOT NULL DEFAULT '',
	expires_at TEXT NOT NULL DEFAULT '',
	accepted_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_invites_org ON invites(org_id);

CREATE TABLE IF NOT EXISTS password_resets (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at TEXT NOT NULL DEFAULT '',
	used_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);

CREATE TABLE IF NOT EXISTS email_verifications (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at TEXT NOT NULL DEFAULT '',
	used_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);

-- Server-side sessions: the cookie stays HMAC-signed (auth.Session), and each
-- issuance inserts a row so logout/revoke/password-change kill it. org_id pins
-- the active org for multi-org users (switchable while a member).
CREATE TABLE IF NOT EXISTS sessions (
	id TEXT PRIMARY KEY,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	token_hash TEXT NOT NULL UNIQUE,
	expires_at TEXT NOT NULL DEFAULT '',
	revoked_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_sessions_user ON sessions(user_id);

-- Fail-closed RLS: drop the permissive ''/NULL bridge arms. From here on,
-- app-role connections without SET LOCAL app.org_id see nothing.
DROP POLICY IF EXISTS org_isolation ON contacts;
CREATE POLICY org_isolation ON contacts
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON groups;
CREATE POLICY org_isolation ON groups
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON campaigns;
CREATE POLICY org_isolation ON campaigns
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON campaign_recipients;
CREATE POLICY org_isolation ON campaign_recipients
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON templates;
CREATE POLICY org_isolation ON templates
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON orgs;
CREATE POLICY org_isolation ON orgs
	USING (id = current_setting('app.org_id', true))
	WITH CHECK (id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON contact_groups;
CREATE POLICY org_isolation ON contact_groups
	USING (
		EXISTS (
			SELECT 1 FROM contacts c JOIN groups g
				ON g.id = contact_groups.group_id
			WHERE c.id = contact_groups.contact_id
				AND c.org_id = current_setting('app.org_id', true)
				AND g.org_id = current_setting('app.org_id', true)
		)
	)
	WITH CHECK (
		EXISTS (
			SELECT 1 FROM contacts c JOIN groups g
				ON g.id = contact_groups.group_id
			WHERE c.id = contact_groups.contact_id
				AND c.org_id = current_setting('app.org_id', true)
				AND g.org_id = current_setting('app.org_id', true)
		)
	);

-- Runtime grants for the new tables (explicit per-table convention from 003).
GRANT SELECT, INSERT, UPDATE, DELETE ON
	users, roles, memberships, wa_account_grants, invites,
	password_resets, email_verifications, sessions
	TO wam_app;
