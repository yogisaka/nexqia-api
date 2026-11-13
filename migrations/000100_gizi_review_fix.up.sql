-- Temuan review SEDANG (race duplikat hook MST): hasil skrining hook
-- idempoten per (form_instance_id, instrument) juga di level DB — partial
-- UNIQUE index hanya untuk baris hasil hook (form_instance_id terisi);
-- baris lain (instrument 'rules' tanpa form) tidak terdampak.
CREATE UNIQUE INDEX IF NOT EXISTS uq_nsr_form_instrument
    ON clinical.nutrition_screening_result (form_instance_id, instrument)
    WHERE form_instance_id IS NOT NULL;
