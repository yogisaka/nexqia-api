-- migrations/000090_physician_active_unique.up.sql
-- Ganti UNIQUE plain (mengikat baris soft-delete; 000007:20) dengan partial
-- unique index: hanya baris AKTIF yang unik per (merchant_id, person_id) —
-- re-create physician pasca soft-delete menjadi legal.
ALTER TABLE core.physician DROP CONSTRAINT physician_merchant_id_person_id_key;
CREATE UNIQUE INDEX physician_merchant_person_active_uidx
    ON core.physician (merchant_id, person_id)
    WHERE deleted_at IS NULL;
