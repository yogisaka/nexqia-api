BEGIN;

DELETE FROM core.template_role_permission trp
USING core.permission p
WHERE trp.permission_id = p.id AND p.code IN ('patient.document.view', 'patient.document.manage');
DELETE FROM core.role_permission rp
USING core.permission p
WHERE rp.permission_id = p.id AND p.code IN ('patient.document.view', 'patient.document.manage');
DELETE FROM core.permission WHERE code IN ('patient.document.view', 'patient.document.manage');

DROP TABLE IF EXISTS core.registration_document_requirement;
DROP TABLE IF EXISTS core.person_document;
DROP TABLE IF EXISTS core.document_type;

UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

COMMIT;
