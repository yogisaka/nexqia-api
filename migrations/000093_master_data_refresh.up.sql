-- nexqia-api/migrations/000093_master_data_refresh.up.sql
-- Spec 2026-11-04-master-data-terminologi-refresh-design §2 (revisi v2):
-- kewarganegaraan WNI/WNA legacy diganti ISO 3166-1. TIDAK menyentuh:
-- code_system option/* lain (INTERIM, docs/31), region legacy
-- http://www.kemendagri.go.id/region (K11), specialty (K5), sistem ICD
-- (K6), urn:nexqia:valueset:demographic (seed demo).
BEGIN;

-- A. anchor ISO 3166-1 (dilengkapi 249 negara oleh
-- `make import-masterdata ARGS=citizenship`); UUID blok 0101 bebas dari
-- seed 001_core_seed (blok 0002/0003-…-001..022).
INSERT INTO terminology.code_system (id, system_uri, name, version, license, attribution)
VALUES ('00000000-0000-0000-0002-000000000101', 'urn:iso:std:iso:3166:-1', 'citizenship',
        'iso-3166-1', 'ISO 3166-1 Country Codes',
        'Turunan tabel resmi ISO 3166 Maintenance Agency (lukes/ISO-3166-Countries-with-Regional-Codes)')
ON CONFLICT (system_uri) DO NOTHING;

INSERT INTO terminology.concept (id, code_system_id, code, display)
VALUES ('00000000-0000-0000-0003-000000000101',
        '00000000-0000-0000-0002-000000000101', 'ID', 'Indonesia')
ON CONFLICT (code_system_id, code) DO NOTHING;

-- B. remap WNI -> anchor, WNA/sisa -> NULL (opsi citizenship legacy saja).
UPDATE core.person p SET citizenship_concept_id = CASE
    WHEN c.display ~* '(^|[^[:alpha:]])(wni|warga negara indonesia)'
    THEN '00000000-0000-0000-0003-000000000101'::uuid ELSE NULL END
FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE p.citizenship_concept_id = c.id
  AND cs.system_uri = 'http://nexqia.local/option/citizenship';

-- C. konsep legacy yang dihapus: HANYA option/citizenship.
CREATE TEMP TABLE doomed_concept ON COMMIT DROP AS
SELECT c.id
FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE cs.system_uri = 'http://nexqia.local/option/citizenship';

-- D. hapus urut FK (concept_map defensif: map legacy mungkin menyentuh).
DELETE FROM terminology.concept_map m USING doomed_concept d
 WHERE m.source_concept_id = d.id OR m.target_concept_id = d.id;
DELETE FROM terminology.concept_hierarchy h USING doomed_concept d
 WHERE h.parent_concept_id = d.id OR h.child_concept_id = d.id;
DELETE FROM terminology.concept c USING doomed_concept d WHERE c.id = d.id;
DELETE FROM terminology.code_system
 WHERE system_uri = 'http://nexqia.local/option/citizenship';

COMMIT;
