-- 002_orgs_rls: multi-tenant foundation for SaaS.
-- Every tenant table gains org_id; RLS scopes rows to app.org_id.
-- Permissive-when-unset ('' = no scoping) keeps the pre-UMS single-tenant
-- server working; feat/ums tightens the policy to fail-closed.
CREATE TABLE IF NOT EXISTS orgs (
	id TEXT PRIMARY KEY,
	name TEXT NOT NULL,
	slug TEXT NOT NULL UNIQUE,
	created_at TEXT NOT NULL DEFAULT (to_char(now() AT TIME ZONE 'UTC', 'YYYY-MM-DD"T"HH24:MI:SS.MS"Z"'))
);
INSERT INTO orgs (id, name, slug)
	VALUES ('org_default', 'Default', 'default')
	ON CONFLICT (id) DO NOTHING;

ALTER TABLE contacts ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT 'org_default';
ALTER TABLE groups ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT 'org_default';
ALTER TABLE campaigns ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT 'org_default';
ALTER TABLE campaign_recipients ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT 'org_default';
ALTER TABLE templates ADD COLUMN IF NOT EXISTS org_id TEXT NOT NULL DEFAULT 'org_default';

-- contact_groups is covered by 004_contact_groups_rls.sql (EXISTS-based policy,
-- no org_id column). Uniqueness becomes per-org (SQLite legacy was global).
ALTER TABLE contacts DROP CONSTRAINT IF EXISTS contacts_phone_key;
DO $$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'contacts_org_phone_key') THEN
		ALTER TABLE contacts ADD CONSTRAINT contacts_org_phone_key UNIQUE (org_id, phone);
	END IF;
END $$;
ALTER TABLE groups DROP CONSTRAINT IF EXISTS groups_name_key;
DO $$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'groups_org_name_key') THEN
		ALTER TABLE groups ADD CONSTRAINT groups_org_name_key UNIQUE (org_id, name);
	END IF;
END $$;
ALTER TABLE templates DROP CONSTRAINT IF EXISTS templates_name_key;
DO $$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_constraint WHERE conname = 'templates_org_name_key') THEN
		ALTER TABLE templates ADD CONSTRAINT templates_org_name_key UNIQUE (org_id, name);
	END IF;
END $$;

CREATE INDEX IF NOT EXISTS idx_contacts_org ON contacts(org_id);
CREATE INDEX IF NOT EXISTS idx_groups_org ON groups(org_id);
CREATE INDEX IF NOT EXISTS idx_campaigns_org ON campaigns(org_id);
CREATE INDEX IF NOT EXISTS idx_recipients_org ON campaign_recipients(org_id);
CREATE INDEX IF NOT EXISTS idx_templates_org ON templates(org_id);

-- RLS: ENABLE + FORCE so the table owner is also scoped. (FORCE does NOT
-- bind superusers — they bypass RLS unconditionally. Tenant isolation bites
-- only for the least-privilege runtime role; see 003_app_role.sql.)
-- feat/devops-obs splits boot-migration (owner DSN) from runtime (app DSN).
ALTER TABLE contacts FORCE ROW LEVEL SECURITY;
ALTER TABLE groups FORCE ROW LEVEL SECURITY;
ALTER TABLE campaigns FORCE ROW LEVEL SECURITY;
ALTER TABLE campaign_recipients FORCE ROW LEVEL SECURITY;
ALTER TABLE templates FORCE ROW LEVEL SECURITY;
ALTER TABLE contacts ENABLE ROW LEVEL SECURITY;
ALTER TABLE groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaigns ENABLE ROW LEVEL SECURITY;
ALTER TABLE campaign_recipients ENABLE ROW LEVEL SECURITY;
ALTER TABLE templates ENABLE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS org_isolation ON contacts;
CREATE POLICY org_isolation ON contacts
	USING (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true))
	WITH CHECK (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON groups;
CREATE POLICY org_isolation ON groups
	USING (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true))
	WITH CHECK (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON campaigns;
CREATE POLICY org_isolation ON campaigns
	USING (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true))
	WITH CHECK (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON campaign_recipients;
CREATE POLICY org_isolation ON campaign_recipients
	USING (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true))
	WITH CHECK (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true));
DROP POLICY IF EXISTS org_isolation ON templates;
CREATE POLICY org_isolation ON templates
	USING (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true))
	WITH CHECK (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR org_id = current_setting('app.org_id', true));
