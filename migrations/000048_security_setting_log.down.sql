-- nexqia-api/migrations/000048_security_setting_log.down.sql
DROP TRIGGER IF EXISTS trg_security_setting_log ON core.feature_flag;
DROP FUNCTION IF EXISTS core.trg_security_setting_log();
DROP TABLE IF EXISTS core.security_setting_log;