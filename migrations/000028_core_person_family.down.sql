-- migrations/000028_core_person_family.down.sql
-- Symmetric rollback of 000028_core_person_family.up.sql.

DROP POLICY IF EXISTS person_family_isolation ON core.person_family;
DROP TRIGGER IF EXISTS trg_person_family_touch ON core.person_family;
DROP INDEX IF EXISTS core.idx_person_family_person;
DROP TABLE IF EXISTS core.person_family;

ALTER TABLE core.person DROP COLUMN IF EXISTS blood_type_concept_id;
ALTER TABLE core.person DROP COLUMN IF EXISTS marital_status_concept_id;
ALTER TABLE core.person DROP COLUMN IF EXISTS job_concept_id;
ALTER TABLE core.person DROP COLUMN IF EXISTS identity_type_concept_id;
ALTER TABLE core.person DROP COLUMN IF EXISTS citizenship_concept_id;
ALTER TABLE core.person DROP COLUMN IF EXISTS region_village_concept_id;
