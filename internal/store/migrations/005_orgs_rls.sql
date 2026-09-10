-- 005_orgs_rls: stop tenant enumeration via the orgs table.
--
-- 003 grants the app role SELECT on orgs but no RLS scoped it, so any
-- org context could list every tenant (id/name/slug). The policy mirrors the
-- pre-UMS bridge: unset app.org_id sees all rows (bootstrap needs
-- DefaultOrg before any org context exists); once set, only the current org.
-- feat/ums tightens this file's policy alongside 002/004 to fail-closed.
ALTER TABLE orgs ENABLE ROW LEVEL SECURITY;
ALTER TABLE orgs FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS org_isolation ON orgs;
CREATE POLICY org_isolation ON orgs
	USING (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR id = current_setting('app.org_id', true))
	WITH CHECK (current_setting('app.org_id', true) = '' OR current_setting('app.org_id', true) IS NULL OR id = current_setting('app.org_id', true));
