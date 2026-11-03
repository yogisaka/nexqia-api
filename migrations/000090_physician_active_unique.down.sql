-- migrations/000090_physician_active_unique.down.sql
-- Best effort: gagal bila terdapat >1 baris per (merchant_id, person_id)
-- (aktif + soft-delete lama). Tidak pernah dijalankan otomatis.
DROP INDEX core.physician_merchant_person_active_uidx;
ALTER TABLE core.physician
    ADD CONSTRAINT physician_merchant_id_person_id_key UNIQUE (merchant_id, person_id);
