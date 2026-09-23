-- migrations/000030_operations_admission_fields.up.sql
-- PRECISE Epic — Admisi rawat jalan: kolom tambahan untuk diagnosis, rujukan, pendamping.

ALTER TABLE operations.admission ADD COLUMN diagnosis_text text;
ALTER TABLE operations.admission ADD COLUMN treatment_barriers text;
ALTER TABLE operations.admission ADD COLUMN special_patient_type text;
ALTER TABLE operations.admission ADD COLUMN needs_companion boolean NOT NULL DEFAULT false;
ALTER TABLE operations.admission ADD COLUMN companion_name text;
ALTER TABLE operations.admission ADD COLUMN referral_origin text;
