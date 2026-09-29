-- nexqia-api/migrations/000047_child_table_tenant_rls.down.sql
DROP POLICY IF EXISTS person_merge_log_isolation ON core.person_merge_log;
ALTER TABLE core.person_merge_log DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS media_link_isolation ON core.media_link;
ALTER TABLE core.media_link DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS admission_guarantor_isolation ON operations.admission_guarantor;
ALTER TABLE operations.admission_guarantor DISABLE ROW LEVEL SECURITY;
DROP POLICY IF EXISTS queue_status_history_isolation ON operations.queue_status_history;
ALTER TABLE operations.queue_status_history DISABLE ROW LEVEL SECURITY;

DROP TRIGGER IF EXISTS trg_00_fill_tenant ON core.person_merge_log;
DROP TRIGGER IF EXISTS trg_00_fill_tenant ON core.media_link;
DROP TRIGGER IF EXISTS trg_00_fill_tenant ON operations.admission_guarantor;
DROP TRIGGER IF EXISTS trg_00_fill_tenant ON operations.queue_status_history;
DROP FUNCTION IF EXISTS core.trg_fill_tenant_person_merge_log();
DROP FUNCTION IF EXISTS core.trg_fill_tenant_media_link();
DROP FUNCTION IF EXISTS operations.trg_fill_tenant_admission_guarantor();
DROP FUNCTION IF EXISTS operations.trg_fill_tenant_queue_status_history();

ALTER TABLE core.person_merge_log DROP COLUMN IF EXISTS company_id;
ALTER TABLE core.media_link DROP COLUMN IF EXISTS merchant_id, DROP COLUMN IF EXISTS company_id;
ALTER TABLE operations.admission_guarantor DROP COLUMN IF EXISTS merchant_id, DROP COLUMN IF EXISTS company_id;
ALTER TABLE operations.queue_status_history DROP COLUMN IF EXISTS merchant_id, DROP COLUMN IF EXISTS company_id;