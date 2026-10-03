-- Departments already copied into merchants stay (copies, not links).
DROP TABLE IF EXISTS core.department_code_map;
DROP FUNCTION IF EXISTS core.trg_fill_tenant_department_code_map();
DROP TABLE IF EXISTS core.template_department_code_map;
DROP TABLE IF EXISTS core.template_department;
DELETE FROM core.template_application WHERE template_id IN (SELECT id FROM core.template WHERE kind = 'department');
DELETE FROM core.template WHERE kind = 'department';
ALTER TABLE core.template DROP CONSTRAINT IF EXISTS chk_template_department_usage;
ALTER TABLE core.template DROP CONSTRAINT template_kind_check;
ALTER TABLE core.template ADD CONSTRAINT template_kind_check CHECK (kind IN ('role'));
