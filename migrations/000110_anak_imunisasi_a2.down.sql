BEGIN;

ALTER TABLE ref.immunization_schedule DROP COLUMN IF EXISTS gender, DROP COLUMN IF EXISTS area_khusus;
DELETE FROM ref.immunization_schedule WHERE source_version='kia-2024-kmk35-2025';

COMMIT;
