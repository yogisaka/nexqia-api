BEGIN;

UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

DELETE FROM core.template_role_permission WHERE permission_id IN (
    SELECT id FROM core.permission
    WHERE code IN ('integration.status.view', 'integration.config.manage'));

DELETE FROM core.role_permission WHERE permission_id IN (
    SELECT id FROM core.permission
    WHERE code IN ('integration.status.view', 'integration.config.manage'));

DELETE FROM core.permission
WHERE code IN ('integration.status.view', 'integration.config.manage');

COMMIT;
