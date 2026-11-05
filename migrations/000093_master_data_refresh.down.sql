-- nexqia-api/migrations/000093_master_data_refresh.down.sql
-- Down menghapus anchor ISO 3166-1 (refs person ke ISO di-NULL dulu).
-- Edge concept_map/concept_hierarchy yang FK ke concept ISO dibersihkan
-- dulu (FK 000005:27-28, 000062:27-28) agar DB dev yang sudah menjalankan
-- `import-masterdata citizenship` (249 negara) bisa turun tanpa FK
-- violation. Penghapusan option/citizenship TIDAK dibalik: data legacy
-- hanya bisa dibangun ulang dengan tool ETL milik operator (spec
-- 2026-11-04 §2).
BEGIN;

UPDATE core.person SET citizenship_concept_id = NULL
 WHERE citizenship_concept_id IN (
   SELECT c.id FROM terminology.concept c
   JOIN terminology.code_system cs ON cs.id = c.code_system_id
   WHERE cs.system_uri = 'urn:iso:std:iso:3166:-1');

DELETE FROM terminology.concept_map m
 USING terminology.concept c
 WHERE (m.source_concept_id = c.id OR m.target_concept_id = c.id)
   AND c.code_system_id IN (SELECT id FROM terminology.code_system
                             WHERE system_uri = 'urn:iso:std:iso:3166:-1');
DELETE FROM terminology.concept_hierarchy h
 USING terminology.concept c
 WHERE (h.parent_concept_id = c.id OR h.child_concept_id = c.id)
   AND c.code_system_id IN (SELECT id FROM terminology.code_system
                             WHERE system_uri = 'urn:iso:std:iso:3166:-1');
DELETE FROM terminology.concept
 WHERE code_system_id IN (SELECT id FROM terminology.code_system
                           WHERE system_uri = 'urn:iso:std:iso:3166:-1');
DELETE FROM terminology.code_system WHERE system_uri = 'urn:iso:std:iso:3166:-1';

COMMIT;
