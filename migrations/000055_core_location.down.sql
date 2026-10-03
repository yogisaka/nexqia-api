-- nexqia-api/migrations/000055_core_location.down.sql
UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit') AND version > 1;
DELETE FROM core.template_role_permission
WHERE permission_id = (SELECT id FROM core.permission WHERE code = 'core.location.manage');
DELETE FROM core.role_permission
WHERE permission_id = (SELECT id FROM core.permission WHERE code = 'core.location.manage');
DELETE FROM core.permission WHERE code = 'core.location.manage';
DROP TABLE IF EXISTS core.location;
DROP FUNCTION IF EXISTS core.trg_location_parent_check();
