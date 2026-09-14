-- migrations/000006_core_person.down.sql
ALTER TABLE core.app_user DROP CONSTRAINT IF EXISTS fk_user_person;
DROP TABLE IF EXISTS core.person_merge_log;
DROP TABLE IF EXISTS core.patient_allergy;
DROP TABLE IF EXISTS core.person;
