-- migrations/000015_core_refresh_token.down.sql
DROP FUNCTION IF EXISTS core.list_user_merchants(uuid);
DROP TABLE IF EXISTS core.refresh_token;
ALTER TABLE core.company DROP COLUMN IF EXISTS max_concurrent_sessions;
