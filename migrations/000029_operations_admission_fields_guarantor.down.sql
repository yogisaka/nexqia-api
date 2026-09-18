-- migrations/000029_operations_admission_fields_guarantor.down.sql
-- Symmetric rollback of 000029_operations_admission_fields_guarantor.up.sql.

DROP INDEX IF EXISTS operations.idx_admission_guarantor_admission;
DROP TABLE IF EXISTS operations.admission_guarantor;

ALTER TABLE operations.admission DROP COLUMN IF EXISTS note;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS referral_source;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS complaint;
