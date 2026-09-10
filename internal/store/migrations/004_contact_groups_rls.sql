-- 004_contact_groups_rls: close the membership-edge gap left by 002.
--
-- contact_groups carries no org_id; without a policy the app role could
-- read/insert/delete arbitrary membership edges across tenants. The policy
-- scopes an edge to the current org only when BOTH ends belong to it.
-- Permissive-when-unset mirrors 002 (pre-UMS bridge: no app.org_id set means
-- no scoping); feat/ums tightens both to fail-closed with SET LOCAL.
ALTER TABLE contact_groups ENABLE ROW LEVEL SECURITY;
ALTER TABLE contact_groups FORCE ROW LEVEL SECURITY;

DROP POLICY IF EXISTS org_isolation ON contact_groups;
CREATE POLICY org_isolation ON contact_groups
	USING (
		current_setting('app.org_id', true) = ''
		OR current_setting('app.org_id', true) IS NULL
		OR EXISTS (
			SELECT 1 FROM contacts c JOIN groups g
				ON g.id = contact_groups.group_id
			WHERE c.id = contact_groups.contact_id
				AND c.org_id = current_setting('app.org_id', true)
				AND g.org_id = current_setting('app.org_id', true)
		)
	)
	WITH CHECK (
		current_setting('app.org_id', true) = ''
		OR current_setting('app.org_id', true) IS NULL
		OR EXISTS (
			SELECT 1 FROM contacts c JOIN groups g
				ON g.id = contact_groups.group_id
			WHERE c.id = contact_groups.contact_id
				AND c.org_id = current_setting('app.org_id', true)
				AND g.org_id = current_setting('app.org_id', true)
		)
	);
