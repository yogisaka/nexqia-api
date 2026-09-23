-- grants schema operations + default privileges + existing objects to app_runtime
GRANT USAGE ON SCHEMA operations TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA operations GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA operations GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA operations TO app_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA operations TO app_runtime;