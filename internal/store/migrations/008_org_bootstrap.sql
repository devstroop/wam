-- 008_org_bootstrap: allow org creation without org context.
--
-- Self-serve signup creates an org before any org context exists, and
-- fail-closed RLS (007) would deny the INSERT. INSERT stays open (only the
-- caller's own name/slug); SELECT/UPDATE/DELETE remain fail-closed, so no
-- tenant data leaks. Abuse-gating (rate limits, CAPTCHA) belongs at the app
-- edge — see feat/devops-obs.
DROP POLICY IF EXISTS org_bootstrap_insert ON orgs;
CREATE POLICY org_bootstrap_insert ON orgs FOR INSERT WITH CHECK (true);
GRANT INSERT ON orgs TO wam_app;
