-- 013_ops: audit trail, API keys, outbound webhooks.
--
-- audit_logs: who-did-what (actor, action, entity). Written by handlers on
-- auth, team, account, campaign and billing changes. No RLS (like
-- memberships): always filtered by session org explicitly; admins list.
-- api_keys: Bearer credentials for /api/* (prefix for lookup, sha256 hash
-- stored, scopes mirror permissions, expirable, revocable).
-- webhook_endpoints: org-owned outbound targets (HMAC-signed JSON).
-- webhook_deliveries: per-event attempt log (retries inline, see worker).
-- Endpoints carry RLS (org-scoped reads); keys/audit/deliveries are
-- server-mediated by explicit predicates (needed pre-org-context at auth).
CREATE TABLE IF NOT EXISTS audit_logs (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL DEFAULT '',
	actor_id TEXT NOT NULL DEFAULT '',
	actor_email TEXT NOT NULL DEFAULT '',
	action TEXT NOT NULL,
	entity TEXT NOT NULL DEFAULT '',
	entity_id TEXT NOT NULL DEFAULT '',
	meta TEXT NOT NULL DEFAULT '{}',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_audit_org ON audit_logs(org_id, created_at);

CREATE TABLE IF NOT EXISTS api_keys (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	user_id TEXT NOT NULL REFERENCES users(id) ON DELETE CASCADE,
	prefix TEXT NOT NULL,
	key_hash TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	scopes TEXT NOT NULL DEFAULT '[]',
	expires_at TEXT NOT NULL DEFAULT '',
	revoked_at TEXT NOT NULL DEFAULT '',
	last_used_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_api_keys_prefix ON api_keys(prefix);
CREATE INDEX IF NOT EXISTS idx_api_keys_org ON api_keys(org_id);

CREATE TABLE IF NOT EXISTS webhook_endpoints (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	url TEXT NOT NULL,
	secret TEXT NOT NULL DEFAULT '',
	events TEXT NOT NULL DEFAULT '[]',
	created_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_webhook_endpoints_org ON webhook_endpoints(org_id);

CREATE TABLE IF NOT EXISTS webhook_deliveries (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL DEFAULT '',
	endpoint_id TEXT NOT NULL DEFAULT '',
	event TEXT NOT NULL,
	payload TEXT NOT NULL DEFAULT '{}',
	status TEXT NOT NULL DEFAULT 'pending',
	attempts INTEGER NOT NULL DEFAULT 0,
	last_error TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_webhook_deliveries_org ON webhook_deliveries(org_id, created_at);

ALTER TABLE webhook_endpoints ENABLE ROW LEVEL SECURITY;
ALTER TABLE webhook_endpoints FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS org_isolation ON webhook_endpoints;
CREATE POLICY org_isolation ON webhook_endpoints
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));

-- Runtime grants for the new tables (explicit per-table convention from 003).
GRANT SELECT, INSERT, UPDATE, DELETE ON
	audit_logs, api_keys, webhook_endpoints, webhook_deliveries
	TO wam_app;
