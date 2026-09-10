-- Local-compose only: pre-create the least-privilege runtime role required by
-- migrations/003_app_role.sql (which refuses to mint credentials itself).
-- Dev password; production pre-creates the role out-of-band with a secret.
DO $$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wam_app') THEN
		CREATE ROLE wam_app WITH LOGIN PASSWORD 'wam_app';
	END IF;
END $$;
