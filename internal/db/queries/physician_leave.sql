-- internal/db/queries/physician_leave.sql
-- operations.physician_leave (cuti) and its schedule_session marking — spec
-- 2026-10-01-b-physician-schedule-design §3/§4.

-- name: CreatePhysicianLeave :one
INSERT INTO operations.physician_leave
    (company_id, merchant_id, physician_id, date_from, date_to, reason, substitute_physician_id, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
RETURNING *;

-- name: GetPhysicianLeaveByID :one
SELECT * FROM operations.physician_leave WHERE id = $1 AND deleted_at IS NULL;

-- name: ListPhysicianLeaves :many
SELECT * FROM operations.physician_leave
WHERE merchant_id = $1 AND deleted_at IS NULL
  AND ($2::date IS NULL OR date_to >= $2)
  AND ($3::date IS NULL OR date_from <= $3)
ORDER BY date_from, physician_id;

-- name: SoftDeletePhysicianLeave :exec
UPDATE operations.physician_leave
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetMerchantPhysician :one
SELECT * FROM core.physician WHERE id = $1 AND merchant_id = $2 AND deleted_at IS NULL;

-- name: UpsertScheduleSessionStatus :one
-- Mark (or restore) the stored session of a pattern for one date with a leave
-- status. The pattern row supplies company/department/times/quotas.
INSERT INTO operations.schedule_session
    (company_id, merchant_id, schedule_id, session_date, physician_id, department_id,
     room_id, start_time, end_time, quota_jkn, slot_quota, status, leave_id, created_by, updated_by)
SELECT ps.company_id, ps.merchant_id, ps.id, sqlc.arg('session_date')::date,
       sqlc.arg('effective_physician_id'), ps.department_id,
       ps.room_id, ps.start_time, ps.end_time, ps.quota_jkn, ps.slot_quota,
       sqlc.arg('status'), sqlc.arg('leave_id'), sqlc.arg('user_id'), sqlc.arg('user_id')
FROM operations.physician_schedule ps
WHERE ps.id = sqlc.arg('schedule_id')
ON CONFLICT (schedule_id, session_date) DO UPDATE
    SET physician_id = EXCLUDED.physician_id, status = EXCLUDED.status,
        leave_id = EXCLUDED.leave_id, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: ListLeaveScheduleSessions :many
SELECT * FROM operations.schedule_session WHERE leave_id = $1;

-- name: RestoreLeaveSessions :execrows
-- Return every session marked by a leave to its pattern physician.
UPDATE operations.schedule_session ss
SET status = 'active', physician_id = ps.physician_id, leave_id = NULL, updated_by = $2
FROM operations.physician_schedule ps
WHERE ss.leave_id = $1 AND ps.id = ss.schedule_id;

-- name: DeleteSessionIfMatchesPattern :execrows
-- Delete a restored session row only when it carries no other override (all
-- overridable fields equal the pattern) and has no registered patients.
DELETE FROM operations.schedule_session ss
USING operations.physician_schedule ps
WHERE ss.id = sqlc.arg('session_id') AND ps.id = ss.schedule_id
  AND ss.room_id IS NOT DISTINCT FROM ps.room_id
  AND ss.start_time = ps.start_time AND ss.end_time = ps.end_time
  AND ss.quota_jkn = ps.quota_jkn AND ss.slot_quota = ps.slot_quota
  AND ss.notes IS NOT DISTINCT FROM ps.notes
  AND NOT EXISTS (
      SELECT 1 FROM operations.admission a
      WHERE a.schedule_id = ss.schedule_id AND a.status <> 'cancelled'
        AND a.admission_type = 'outpatient'
        AND (a.admission_at AT TIME ZONE sqlc.arg('tz')::text)::date = ss.session_date);

-- name: ListLeaveAffectedAdmissions :many
-- Outpatient admissions of one physician's patterns inside a date range that
-- are not cancelled — the "pasien terdampak" list (spec §4).
SELECT a.id, a.person_id, per.full_name AS patient_name,
       (a.admission_at AT TIME ZONE sqlc.arg('tz')::text)::date AS visit_date,
       ss.start_time AS session_start, ss.end_time AS session_end,
       pay.name AS payer_name, COALESCE(pay.payer_type, 'self_pay') AS payer_type
FROM operations.admission a
JOIN core.person per ON per.id = a.person_id
JOIN operations.physician_schedule ps ON ps.id = a.schedule_id
LEFT JOIN operations.schedule_session ss
       ON ss.schedule_id = a.schedule_id
      AND ss.session_date = (a.admission_at AT TIME ZONE sqlc.arg('tz')::text)::date
LEFT JOIN core.payer pay ON pay.id = a.primary_payer_id
WHERE a.merchant_id = sqlc.arg('merchant_id')
  AND ps.physician_id = sqlc.arg('physician_id') AND ps.deleted_at IS NULL
  AND a.admission_type = 'outpatient' AND a.status <> 'cancelled' AND a.deleted_at IS NULL
  AND (a.admission_at AT TIME ZONE sqlc.arg('tz')::text)::date
      BETWEEN sqlc.arg('date_from')::date AND sqlc.arg('date_to')::date
ORDER BY visit_date, session_start;

-- name: ResolveLeaveAdmission :one
-- Partial update used by the leave resolution actions: only the COALESCEd
-- columns whose parameter is not NULL are written.
UPDATE operations.admission
SET physician_id = COALESCE(sqlc.arg('physician_id'), physician_id),
    status = COALESCE(NULLIF(sqlc.arg('status'), ''), status),
    note = COALESCE(sqlc.arg('note'), note),
    updated_by = sqlc.arg('user_id')
WHERE id = sqlc.arg('admission_id') AND deleted_at IS NULL
RETURNING *;

-- name: ListStoredActiveSessionsBetween :many
-- Stored session overrides to copy in copy-week (only active rows).
SELECT * FROM operations.schedule_session
WHERE merchant_id = $1 AND status = 'active'
  AND session_date >= $2 AND session_date <= $3
ORDER BY session_date, start_time;
