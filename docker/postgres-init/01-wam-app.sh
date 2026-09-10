#!/bin/sh
# Local-compose only: pre-create the least-privilege runtime role required by
# migrations/003_app_role.sql (which refuses to mint credentials itself).
# Password comes from POSTGRES_APP_PASSWORD in .env — never committed.
# Production pre-creates the role out-of-band with a secret.
: "${POSTGRES_USER:?POSTGRES_USER must be set}"
: "${POSTGRES_DB:?POSTGRES_DB must be set}"
: "${POSTGRES_APP_PASSWORD:?Set POSTGRES_APP_PASSWORD in .env (see .env.example)}"
psql -v ON_ERROR_STOP=1 --username "$POSTGRES_USER" --dbname "$POSTGRES_DB" <<EOSQL
DO \$\$ BEGIN
	IF NOT EXISTS (SELECT 1 FROM pg_roles WHERE rolname = 'wam_app') THEN
		CREATE ROLE wam_app WITH LOGIN PASSWORD '$POSTGRES_APP_PASSWORD';
	END IF;
END \$\$;
EOSQL
