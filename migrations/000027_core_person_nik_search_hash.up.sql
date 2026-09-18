-- migrations/000027_core_person_nik_search_hash.up.sql
ALTER TABLE core.person ADD COLUMN nik_search_hash text;
CREATE INDEX idx_person_nik_search_hash ON core.person (company_id, nik_search_hash)
    WHERE deleted_at IS NULL AND nik_search_hash IS NOT NULL;