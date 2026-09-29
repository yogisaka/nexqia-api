-- nexqia-api/migrations/000046_audit_trail.down.sql
-- Rows written with changed_by NULL are removed so NOT NULL can be restored.
DROP TRIGGER IF EXISTS trg_audit_queue ON operations.queue;
DROP TRIGGER IF EXISTS trg_audit_admission_guarantor ON operations.admission_guarantor;
DROP TRIGGER IF EXISTS trg_audit_admission ON operations.admission;
DROP TRIGGER IF EXISTS trg_audit_patient_allergy ON core.patient_allergy;
DROP TRIGGER IF EXISTS trg_audit_person_family ON core.person_family;
DROP TRIGGER IF EXISTS trg_audit_person ON core.person;
DROP FUNCTION IF EXISTS core.trg_audit_row();
DROP INDEX IF EXISTS core.idx_access_log_resource;
DROP INDEX IF EXISTS core.idx_audit_log_company_time;
ALTER TABLE core.access_log DROP COLUMN IF EXISTS status_code;
ALTER TABLE core.access_log DROP COLUMN IF EXISTS platform_admin_id;
ALTER TABLE core.audit_log DROP COLUMN IF EXISTS platform_admin_id;
ALTER TABLE core.audit_log DROP COLUMN IF EXISTS changed_fields;
DELETE FROM core.audit_log WHERE changed_by IS NULL;
ALTER TABLE core.audit_log ALTER COLUMN changed_by SET NOT NULL;
ALTER TABLE core.audit_log ADD COLUMN new_value jsonb;
ALTER TABLE core.audit_log ADD COLUMN old_value jsonb;
