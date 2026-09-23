-- migrations/000030_operations_admission_fields.down.sql
-- PRECISE Epic — Rollback admission fields.

ALTER TABLE operations.admission DROP COLUMN IF EXISTS diagnosis_text;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS treatment_barriers;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS special_patient_type;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS needs_companion;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS companion_name;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS referral_origin;
