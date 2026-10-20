BEGIN;

ALTER TABLE core.merchant DROP COLUMN IF EXISTS hospital_class;

COMMIT;
