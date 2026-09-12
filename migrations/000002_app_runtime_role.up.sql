-- Infra plumbing, not a pillar DDL transcript (13-core-foundation-spec.md §2): non-owner role
-- for the app's pgx connection pool. FORCE ROW LEVEL SECURITY on future tenant tables only
-- takes effect if the app actually connects as this role, never as the migration/owner role.
CREATE ROLE app_runtime LOGIN PASSWORD 'app_runtime_dev_password' NOSUPERUSER NOCREATEDB NOCREATEROLE NOBYPASSRLS;

GRANT USAGE ON SCHEMA core, terminology TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA core GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA terminology GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA core GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA terminology GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;
