-- nexqia-api/migrations/000045_mfa_grace.down.sql
DROP TABLE IF EXISTS platform.admin_mfa_recovery_code;
ALTER TABLE platform.admin_user DROP COLUMN IF EXISTS mfa_grace_until, DROP COLUMN IF EXISTS mfa_secret;
ALTER TABLE core.app_user DROP COLUMN IF EXISTS mfa_grace_until;
