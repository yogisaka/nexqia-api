-- migrations/000031_core_patient_allergy_extension.up.sql
-- PRECISE Epic — Riwayat Alergi: tambahkan efek samping dan tanggal kejadian.

ALTER TABLE core.patient_allergy ADD COLUMN effect_side text;
ALTER TABLE core.patient_allergy ADD COLUMN event_date date;
