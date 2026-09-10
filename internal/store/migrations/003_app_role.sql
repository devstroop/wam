-- 003_app_role: least-privilege runtime role for RLS enforcement.
--
-- Superusers (e.g. the default postgres user) bypass RLS unconditionally —
-- FORCE ROW LEVEL SECURITY only binds table owners. So the app must serve via
-- a NON-superuser role for tenant isolation to bite (server uses
-- WAM_APP_DATABASE_URL for runtime, WAM_DATABASE_URL/owner for migrations).
--
-- The role is NOT created here: pre-create it out-of-band with a strong
-- password (local compose does this via docker/postgres-init; production via
-- secrets management). This migration fails loudly otherwise instead of
-- minting a weak default credential.
--
-- Grants are scoped to explicit app tables — never ON ALL TABLES (that would
-- leak schema_migrations and whatsmeow session tables). Convention: every
-- future migration that adds a table ends with its GRANT ... TO wam_app.
DO $$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wam_app') THEN
		RAISE EXCEPTION 'role wam_app missing: pre-create it out-of-band (see docker/postgres-init for the dev statement)';
	END IF;
END $$;

GRANT USAGE ON SCHEMA public TO wam_app;
GRANT SELECT ON orgs TO wam_app;
GRANT SELECT, INSERT, UPDATE, DELETE ON
	contacts, groups, contact_groups, campaigns, campaign_recipients, templates
	TO wam_app;
-- No sequence grants: ids are client-generated TEXT. No ALTER DEFAULT
-- PRIVILEGES: future tables grant explicitly per the convention above.
