-- nexqia-api/seed/002_legacy_terminology_cleanup.sql
-- Dijalankan `make seed-legacy-terminology` SETELAH restore dump INTERIM
-- (data-import/legacy-master-terminology.sql.gz): dump memuat kembali
-- code_system legacy "citizenship" (WNI/WNA) yang bertabrakan nama dengan
-- anchor ISO 3166-1 (migration 000093) di resolver by-name FE/API.
-- Idempotent; hanya untuk server development (spec §4).
BEGIN;

INSERT INTO terminology.code_system (id, system_uri, name, version, license, attribution)
VALUES ('00000000-0000-0000-0002-000000000101', 'urn:iso:std:iso:3166:-1', 'citizenship',
        'iso-3166-1', 'ISO 3166-1 Country Codes',
        'Turunan tabel resmi ISO 3166 Maintenance Agency (lukes/ISO-3166-Countries-with-Regional-Codes)')
ON CONFLICT (system_uri) DO NOTHING;

INSERT INTO terminology.concept (id, code_system_id, code, display)
VALUES ('00000000-0000-0000-0003-000000000101',
        '00000000-0000-0000-0002-000000000101', 'ID', 'Indonesia')
ON CONFLICT (code_system_id, code) DO NOTHING;

UPDATE core.person p SET citizenship_concept_id = CASE
    WHEN c.display ~* '(^|[^[:alpha:]])(wni|warga negara indonesia)'
    THEN '00000000-0000-0000-0003-000000000101' ELSE NULL END
FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE p.citizenship_concept_id = c.id
  AND cs.system_uri = 'http://nexqia.local/option/citizenship';

CREATE TEMP TABLE doomed_concept ON COMMIT DROP AS
SELECT c.id
FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE cs.system_uri = 'http://nexqia.local/option/citizenship';

DELETE FROM terminology.concept_map m USING doomed_concept d
 WHERE m.source_concept_id = d.id OR m.target_concept_id = d.id;
DELETE FROM terminology.concept_hierarchy h USING doomed_concept d
 WHERE h.parent_concept_id = d.id OR h.child_concept_id = d.id;
DELETE FROM terminology.concept c USING doomed_concept d WHERE c.id = d.id;
DELETE FROM terminology.code_system
 WHERE system_uri = 'http://nexqia.local/option/citizenship';

COMMIT;
