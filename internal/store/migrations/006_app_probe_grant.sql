-- 006_app_probe_grant: let the runtime role run its boot probe.
--
-- OpenPostgresNoMigrate fails fast with SELECT COUNT(*) FROM
-- schema_migrations. That needs SELECT on the version table (metadata only:
-- migration filenames, no tenant data). Convention from 003 holds: explicit
-- per-table grants, no blanket privileges.
GRANT SELECT ON schema_migrations TO wam_app;
