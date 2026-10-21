BEGIN;

DELETE FROM core.template_role_permission trp
USING core.permission p
WHERE trp.permission_id = p.id
  AND p.code IN ('quality.incident.view', 'quality.incident.manage', 'quality.incident.report');
DELETE FROM core.role_permission rp
USING core.permission p
WHERE rp.permission_id = p.id
  AND p.code IN ('quality.incident.view', 'quality.incident.manage', 'quality.incident.report');
DELETE FROM core.permission
WHERE code IN ('quality.incident.view', 'quality.incident.manage', 'quality.incident.report');

DROP SCHEMA IF EXISTS quality CASCADE;

UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

COMMIT;
