-- internal/db/queries/schedule_session.sql
-- operations.schedule_session per-date overrides (spec
-- 2026-10-01-b-physician-schedule-design §3) and per-payer registration counts.

-- name: ListScheduleSessionsBetween :many
SELECT ss.id, ss.company_id, ss.merchant_id, ss.schedule_id, ss.session_date,
       ss.physician_id, ss.department_id, ss.room_id, ss.start_time, ss.end_time,
       ss.quota_jkn, ss.slot_quota, ss.status, ss.leave_id, ss.notes,
       ss.bpjs_sync_status,
       lv.reason AS leave_reason,
       lv.substitute_physician_id AS leave_substitute_physician_id
FROM operations.schedule_session ss
LEFT JOIN operations.physician_leave lv ON lv.id = ss.leave_id
WHERE ss.merchant_id = $1 AND ss.session_date >= $2 AND ss.session_date <= $3
ORDER BY ss.session_date, ss.start_time;

-- name: GetScheduleSessionByScheduleAndDate :one
SELECT * FROM operations.schedule_session WHERE schedule_id = $1 AND session_date = $2;

-- name: UpsertScheduleSession :one
INSERT INTO operations.schedule_session
    (company_id, merchant_id, schedule_id, session_date, physician_id, department_id,
     room_id, start_time, end_time, quota_jkn, slot_quota, notes, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $13)
ON CONFLICT (schedule_id, session_date) DO UPDATE
    SET room_id = EXCLUDED.room_id, start_time = EXCLUDED.start_time,
        end_time = EXCLUDED.end_time, quota_jkn = EXCLUDED.quota_jkn,
        slot_quota = EXCLUDED.slot_quota, notes = EXCLUDED.notes,
        updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: DeleteScheduleSession :exec
DELETE FROM operations.schedule_session WHERE id = $1;

-- name: CountSessionAdmissions :one
SELECT count(*) FROM operations.admission
WHERE schedule_id = $1 AND status <> 'cancelled' AND admission_type = 'outpatient'
  AND (admission_at AT TIME ZONE $2::text)::date = $3::date;

-- name: CountScheduleRegistrations :many
SELECT a.schedule_id,
       (a.admission_at AT TIME ZONE $1::text)::date AS session_date,
       count(*) FILTER (WHERE pay.payer_type = 'bpjs') AS registered_jkn,
       count(*) FILTER (WHERE COALESCE(pay.payer_type, '') <> 'bpjs') AS registered_other
FROM operations.admission a
LEFT JOIN core.payer pay ON pay.id = a.primary_payer_id
WHERE a.merchant_id = $2 AND a.schedule_id IS NOT NULL
  AND a.admission_type = 'outpatient' AND a.status <> 'cancelled'
  AND (a.admission_at AT TIME ZONE $1::text)::date BETWEEN $3::date AND $4::date
GROUP BY a.schedule_id, session_date;
