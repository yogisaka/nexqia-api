-- nexqia-api/migrations/000057_queue_flow_display.down.sql
UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit') AND version > 1;
DELETE FROM core.template_role_permission
WHERE permission_id = (SELECT id FROM core.permission WHERE code = 'operations.queue.configure');
DELETE FROM core.role_permission
WHERE permission_id = (SELECT id FROM core.permission WHERE code = 'operations.queue.configure');
DELETE FROM core.permission WHERE code = 'operations.queue.configure';
DROP TABLE IF EXISTS operations.display_board;
DROP INDEX IF EXISTS operations.idx_queue_stage_waiting;
ALTER TABLE operations.queue
    DROP COLUMN IF EXISTS served_by, DROP COLUMN IF EXISTS finished_at, DROP COLUMN IF EXISTS started_at,
    DROP COLUMN IF EXISTS checked_in_at, DROP COLUMN IF EXISTS priority, DROP COLUMN IF EXISTS stage_id,
    DROP COLUMN IF EXISTS journey_id;
DROP TABLE IF EXISTS operations.queue_journey;
ALTER TABLE operations.counter
    DROP COLUMN IF EXISTS binding, DROP COLUMN IF EXISTS location_id, DROP COLUMN IF EXISTS stage_id;
DROP TABLE IF EXISTS operations.queue_stage;
DROP TABLE IF EXISTS operations.queue_flow;
