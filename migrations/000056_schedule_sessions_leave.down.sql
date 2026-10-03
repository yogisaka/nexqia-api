-- nexqia-api/migrations/000056_schedule_sessions_leave.down.sql
DROP INDEX IF EXISTS operations.idx_admission_schedule;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS schedule_id;
ALTER TABLE core.physician DROP COLUMN IF EXISTS sip_valid_until;
DROP TABLE IF EXISTS operations.schedule_session;
DROP TABLE IF EXISTS operations.physician_leave;
ALTER TABLE operations.physician_schedule
    DROP CONSTRAINT IF EXISTS chk_physician_schedule_time,
    DROP CONSTRAINT IF EXISTS chk_physician_schedule_slot_quota,
    DROP COLUMN IF EXISTS notes,
    DROP COLUMN IF EXISTS shift_id,
    DROP COLUMN IF EXISTS service_types,
    DROP COLUMN IF EXISTS minutes_per_patient,
    DROP COLUMN IF EXISTS quota_jkn,
    DROP COLUMN IF EXISTS room_id;
DROP TABLE IF EXISTS operations.shift;
