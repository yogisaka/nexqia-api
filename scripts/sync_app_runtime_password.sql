-- scripts/sync_app_runtime_password.sql
-- Run via `make sync-runtime-password` (needs DATABASE_URL owner conn + APP_RUNTIME_PASSWORD in .env).
-- migrations/000002_app_runtime_role.up.sql creates the app_runtime role with a fixed
-- placeholder password (golang-migrate runs raw SQL, no psql :var substitution available
-- inside .sql migration files) — this script is the out-of-band step that syncs the role's
-- actual password to whatever APP_RUNTIME_PASSWORD is set to in .env. Safe to re-run.
ALTER ROLE app_runtime WITH PASSWORD :'app_runtime_password';
