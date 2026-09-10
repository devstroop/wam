-- 001_base: single-tenant tables, mirroring the legacy SQLite schema.
-- Timestamps stay TEXT (RFC3339 UTC) so Go scan sites are unchanged.
-- org_id arrives in 002;WhatsApp session tables are owned by whatsmeow sqlstore.
CREATE TABLE IF NOT EXISTS contacts (
	id TEXT PRIMARY KEY,
	phone TEXT NOT NULL UNIQUE,
	name TEXT NOT NULL DEFAULT '',
	wa_jid TEXT NOT NULL DEFAULT '',
	wa_ok INTEGER NOT NULL DEFAULT 0,
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_contacts_phone ON contacts(phone);

CREATE TABLE IF NOT EXISTS groups (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	color TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);

CREATE TABLE IF NOT EXISTS contact_groups (
	contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
	group_id TEXT NOT NULL REFERENCES groups(id) ON DELETE CASCADE,
	PRIMARY KEY (contact_id, group_id)
);
CREATE INDEX IF NOT EXISTS idx_contact_groups_group ON contact_groups(group_id);

CREATE TABLE IF NOT EXISTS campaigns (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	status TEXT NOT NULL DEFAULT 'draft',
	body_template TEXT NOT NULL DEFAULT '',
	audience_filter TEXT NOT NULL DEFAULT '{}',
	scheduled_at TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
CREATE INDEX IF NOT EXISTS idx_campaigns_status ON campaigns(status);

CREATE TABLE IF NOT EXISTS campaign_recipients (
	campaign_id TEXT NOT NULL REFERENCES campaigns(id) ON DELETE CASCADE,
	contact_id TEXT NOT NULL REFERENCES contacts(id) ON DELETE CASCADE,
	status TEXT NOT NULL DEFAULT 'queued',
	wa_msg_id TEXT NOT NULL DEFAULT '',
	error TEXT NOT NULL DEFAULT '',
	sent_at TEXT NOT NULL DEFAULT '',
	PRIMARY KEY (campaign_id, contact_id)
);
CREATE INDEX IF NOT EXISTS idx_recipients_status ON campaign_recipients(campaign_id, status);

CREATE TABLE IF NOT EXISTS templates (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL UNIQUE,
	category TEXT NOT NULL DEFAULT 'marketing',
	language TEXT NOT NULL DEFAULT 'en',
	header TEXT NOT NULL DEFAULT '',
	body TEXT NOT NULL,
	footer TEXT NOT NULL DEFAULT '',
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
