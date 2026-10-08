-- nexqia-api/migrations/000065_person_merge.down.sql
UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
DELETE FROM core.template_role tr USING core.template t
WHERE t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role' AND tr.name = 'Rekam Medis';
DELETE FROM core.template_role_permission trp USING core.permission p
WHERE p.id = trp.permission_id AND p.code IN ('core.person.merge_request', 'core.person.merge', 'core.person.unmerge');
DELETE FROM core.role_permission rp USING core.permission p
WHERE p.id = rp.permission_id AND p.code IN ('core.person.merge_request', 'core.person.merge', 'core.person.unmerge');
DELETE FROM core.permission WHERE code IN ('core.person.merge_request', 'core.person.merge', 'core.person.unmerge');
DROP FUNCTION IF EXISTS core.person_unmerge_move(uuid);
DROP FUNCTION IF EXISTS core.person_merge_move(uuid, uuid);
DROP FUNCTION IF EXISTS core.person_merge_counts(uuid);
DROP FUNCTION IF EXISTS core.person_merge_blockers(uuid);
DROP FUNCTION IF EXISTS core.person_merge_assert(uuid[]);
DROP TRIGGER IF EXISTS trg_person_not_merged ON operations.admission;
DROP TRIGGER IF EXISTS trg_person_not_merged ON operations.queue;
DROP TRIGGER IF EXISTS trg_person_not_merged ON operations.queue_journey;
DROP TRIGGER IF EXISTS trg_person_not_merged ON core.patient_allergy;
DROP TRIGGER IF EXISTS trg_person_not_merged ON core.person_family;
DROP FUNCTION IF EXISTS core.trg_person_not_merged();
DROP TABLE IF EXISTS core.person_duplicate_pair;
DROP FUNCTION IF EXISTS core.trg_fill_tenant_person_duplicate_pair();
ALTER TABLE core.patient_allergy DROP COLUMN IF EXISTS needs_review;
DROP INDEX IF EXISTS core.idx_person_merge_log_company;
ALTER TABLE core.person_merge_log
    DROP CONSTRAINT IF EXISTS chk_person_merge_log_unmerge_ref,
    DROP COLUMN IF EXISTS reverted_by, DROP COLUMN IF EXISTS reverted_at,
    DROP COLUMN IF EXISTS moved, DROP COLUMN IF EXISTS snapshot,
    DROP COLUMN IF EXISTS field_choices, DROP COLUMN IF EXISTS verified_documents,
    DROP COLUMN IF EXISTS identity_check, DROP COLUMN IF EXISTS merge_log_id,
    DROP COLUMN IF EXISTS action;
DROP INDEX IF EXISTS core.idx_person_merged_mrn;
ALTER TABLE core.person
    DROP CONSTRAINT IF EXISTS chk_person_merged_not_self,
    DROP CONSTRAINT IF EXISTS chk_person_merged_deleted,
    DROP COLUMN IF EXISTS merged_into_person_id;
