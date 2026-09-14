-- migrations/000012_core_audit_access_log.down.sql
DELETE FROM partman.part_config WHERE parent_table IN ('core.audit_log', 'core.access_log');
DROP TABLE IF EXISTS core.access_log CASCADE;
DROP TABLE IF EXISTS core.audit_log CASCADE;
