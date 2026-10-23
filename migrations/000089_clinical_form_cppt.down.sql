BEGIN;

UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

DELETE FROM core.template_role_permission trp
USING core.template_role tr, core.permission p, core.template t
WHERE trp.template_role_id = tr.id AND trp.permission_id = p.id
  AND tr.template_id = t.id AND t.scope = 'platform' AND t.kind = 'role'
  AND (p.code IN ('clinical.record.write', 'clinical.record.countersign', 'clinical.nutrition.write', 'terminology.lookup')
       OR tr.name = 'Dietisien');

DELETE FROM core.template_role tr
USING core.template t
WHERE tr.template_id = t.id AND t.scope = 'platform' AND t.kind = 'role'
  AND tr.name = 'Dietisien';

DELETE FROM core.role_permission rp
USING core.permission p
WHERE rp.permission_id = p.id
  AND p.code IN ('clinical.record.write', 'clinical.record.countersign', 'clinical.nutrition.write', 'terminology.lookup');

DELETE FROM core.permission
WHERE code IN ('clinical.record.write', 'clinical.record.countersign', 'clinical.nutrition.write', 'terminology.lookup');

DROP TABLE IF EXISTS clinical.nutrition_order;
DROP TABLE IF EXISTS clinical.diagnosis;
DROP TABLE IF EXISTS clinical_form.form_instance;
DROP SCHEMA IF EXISTS clinical_form;
DROP SCHEMA IF EXISTS clinical;

DELETE FROM core.template_clinical_form f
USING core.template t
WHERE f.template_id = t.id AND t.kind = 'clinical_form';
DELETE FROM core.template WHERE kind = 'clinical_form';
DROP TABLE IF EXISTS core.template_clinical_form;

ALTER TABLE core.template DROP CONSTRAINT IF EXISTS chk_template_clinical_form_usage;
ALTER TABLE core.template DROP CONSTRAINT template_kind_check;
ALTER TABLE core.template ADD CONSTRAINT template_kind_check
    CHECK (kind IN ('role', 'department', 'price_list'));

COMMIT;
