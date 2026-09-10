-- 009_wa_accounts: multi-account foundation.
--
-- wa_accounts owns WhatsApp numbers per org. Runtime clients map 1:1 to rows
-- via device_jid (whatsmeow sqlstore device JID; "" until first pairing —
-- unpaired devices don't survive restarts by design, re-pair after reboot).
-- campaigns.wa_account_id pins the sender (NULL = pre-multi-account legacy).
-- wa_account_grants.account_id is now FK-bound (was free text in 007).
--
-- RLS: wa_accounts fail-closed on org_id. Grants follow (org_id = setting):
-- middleware loads grants inside the request org transaction; all other
-- grants access is scoped too. Memberships stay predicate-scoped (needed
-- pre-org-context at login) per the 007 convention.

CREATE TABLE IF NOT EXISTS wa_accounts (
	id TEXT PRIMARY KEY,
	org_id TEXT NOT NULL REFERENCES orgs(id) ON DELETE CASCADE,
	label TEXT NOT NULL DEFAULT '',
	phone TEXT NOT NULL DEFAULT '',
	jid TEXT NOT NULL DEFAULT '',
	device_jid TEXT NOT NULL DEFAULT '',
	status TEXT NOT NULL DEFAULT 'pending',
	last_seen_at TEXT NOT NULL DEFAULT '',
	created_by TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_wa_accounts_org ON wa_accounts(org_id);

ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS wa_account_id TEXT REFERENCES wa_accounts(id) ON DELETE SET NULL;
CREATE INDEX IF NOT EXISTS idx_campaigns_account ON campaigns(wa_account_id);

DO $$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'fk_grants_account') THEN
		ALTER TABLE wa_account_grants
			ADD CONSTRAINT fk_grants_account
			FOREIGN KEY (account_id) REFERENCES wa_accounts(id) ON DELETE CASCADE;
	END IF;
END $$;

ALTER TABLE wa_accounts ENABLE ROW LEVEL SECURITY;
ALTER TABLE wa_accounts FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS org_isolation ON wa_accounts;
CREATE POLICY org_isolation ON wa_accounts
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));

ALTER TABLE wa_account_grants ENABLE ROW LEVEL SECURITY;
ALTER TABLE wa_account_grants FORCE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS org_isolation ON wa_account_grants;
CREATE POLICY org_isolation ON wa_account_grants
	USING (org_id = current_setting('app.org_id', true))
	WITH CHECK (org_id = current_setting('app.org_id', true));

-- Runtime grants for the new table (explicit per-table convention from 003).
GRANT SELECT, INSERT, UPDATE, DELETE ON wa_accounts TO wam_app;
