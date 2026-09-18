-- migrations/000027_core_person_nik_search_hash.down.sql
DROP INDEX IF EXISTS idx_person_nik_search_hash;
ALTER TABLE core.person DROP COLUMN nik_search_hash;