-- Tags group terminology.code_system rows by purpose, so a picker can ask for
-- "every diagnosis standard" instead of hardcoding names (diagnosis-config UI,
-- 2026-09-25). Tag a new standard later without code changes, e.g.:
--   UPDATE terminology.code_system SET tags = tags || '{diagnosis}' WHERE name = 'ICD-11';
ALTER TABLE terminology.code_system ADD COLUMN tags text[] NOT NULL DEFAULT '{}';

-- ICD-10 rows are loaded by the local ETL (cmd/migrate-data), not by
-- migrations or seed. On a DB where the ETL already ran this tags ICD-10; on a
-- fresh DB it matches nothing, and the same UPDATE must be run after the ETL.
UPDATE terminology.code_system SET tags = tags || '{diagnosis}'
WHERE name = 'ICD-10' AND NOT ('diagnosis' = ANY(tags));
