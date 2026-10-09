-- nexqia-api/migrations/000066_person_nik_unique.down.sql
DROP INDEX IF EXISTS core.uq_person_nik_hash;
CREATE UNIQUE INDEX uq_person_nik ON core.person (company_id, nik) WHERE deleted_at IS NULL AND nik IS NOT NULL;
