-- 010_sender_outbox: durable send queue for the campaign worker.
--
-- Backend decision: Postgres outbox, not Redis. Enqueue happens in the same
-- transaction as campaign state changes (no lost or duplicate jobs on
-- crash), claiming uses SELECT ... FOR UPDATE SKIP LOCKED (multi-worker
-- safe when we scale horizontally), and no new infrastructure is required.
-- WAM_REDIS_URL stays reserved for future rate-limit coordination.
--
-- Lifecycle per row: queued (next_at <= now, unlocked) -> claimed
-- (locked_by/by worker id, locked_at heartbeat) -> done (deleted) or
-- retry (attempts+1, next_at = now + backoff) or dead (attempts exhausted:
-- recipient marked failed, row deleted). Pause leaves rows in place
-- (resume reclaims); cancel deletes the campaign's rows.
CREATE TABLE IF NOT EXISTS send_outbox (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
	contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
	account_id TEXT REFERENCES wa_accounts(id) ON DELETE SET NULL,
	attempts INTEGER NOT NULL DEFAULT 0,
	next_at TEXT NOT NULL DEFAULT '',
	locked_by TEXT NOT NULL DEFAULT '',
	locked_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"')),
	UNIQUE (campaign_id, contact_id)
);
CREATE INDEX IF NOT EXISTS idx_outbox_claim ON send_outbox(org_id, next_at, locked_at);
CREATE INDEX IF NOT EXISTS idx_outbox_campaign ON send_outbox(campaign_id);

ALTER TABLE send_outbox ENABLE ROW LEVEL SECURITY;
ALTER TABLE send_outbox FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS org_isolation ON send_outbox;
CREATE POLICY org_isolation ON send_outbox
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));

-- Runtime grants for the new table (explicit per-table convention from 003).
GRANT SELECT, INSERT, UPDATE, DELETE ON send_outbox TO wam_app;
