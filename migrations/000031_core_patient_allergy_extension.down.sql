-- migrations/000031_core_patient_allergy_extension.down.sql
-- PRECISE Epic — Rollback allergy extension.

ALTER TABLE core.patient_allergy DROP COLUMN IF EXISTS effect_side;
ALTER TABLE core.patient_allergy DROP COLUMN IF EXISTS event_date;
