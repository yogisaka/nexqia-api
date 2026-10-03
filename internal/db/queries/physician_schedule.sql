-- internal/db/queries/physician_schedule.sql
-- operations.physician_schedule (docs/08-operations-ddl.md §1 "Penjadwalan & Antrian")
-- Room/quota/shift columns added by migration 000054 (spec
-- 2026-10-01-b-physician-schedule-design §3).

-- name: CreatePhysicianSchedule :one
INSERT INTO operations.physician_schedule
    (company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, slot_quota, effective_from, effective_to, created_by, updated_by,
     room_id, quota_jkn, minutes_per_patient, service_types, shift_id, notes)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11, $12, $13, $14, $15, $16, $17)
RETURNING *;

-- name: GetPhysicianScheduleByID :one
SELECT * FROM operations.physician_schedule WHERE id = $1 AND deleted_at IS NULL;

-- name: ListPhysicianSchedulesByMerchant :many
SELECT * FROM operations.physician_schedule
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY created_at
LIMIT $2 OFFSET $3;

-- name: ListActiveSchedulesForDepartmentToday :many
SELECT * FROM operations.physician_schedule
WHERE merchant_id = $1 AND department_id = $2 AND day_of_week = $3
  AND is_active AND deleted_at IS NULL
  AND effective_from <= $4 AND (effective_to IS NULL OR effective_to >= $4)
ORDER BY start_time;

-- name: UpdatePhysicianSchedule :one
UPDATE operations.physician_schedule
SET day_of_week = $2, start_time = $3, end_time = $4, slot_quota = $5,
    effective_from = $6, effective_to = $7, is_active = $8, updated_by = $9,
    room_id = $10, quota_jkn = $11, minutes_per_patient = $12, service_types = $13,
    shift_id = $14, notes = $15
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePhysicianSchedule :exec
UPDATE operations.physician_schedule
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListSchedulesByMerchantAndDay :many
SELECT * FROM operations.physician_schedule
WHERE merchant_id = $1 AND day_of_week = $2 AND deleted_at IS NULL;

-- name: ListSchedulePatternsForCalendar :many
SELECT s.id, s.merchant_id, s.physician_id, s.department_id, s.room_id, s.shift_id,
       s.day_of_week, s.start_time, s.end_time, s.quota_jkn, s.slot_quota,
       s.effective_from, s.effective_to, s.is_active, s.notes,
       per.full_name AS physician_name, ph.sip_number AS physician_sip_number,
       ph.sip_valid_until AS physician_sip_valid_until,
       d.name AS department_name
FROM operations.physician_schedule s
JOIN core.physician ph ON ph.id = s.physician_id
JOIN core.person per ON per.id = ph.person_id
JOIN core.department d ON d.id = s.department_id
WHERE s.merchant_id = $1 AND s.deleted_at IS NULL
ORDER BY s.start_time;

-- name: GetPhysicianSipValidUntil :one
SELECT sip_valid_until FROM core.physician WHERE id = $1 AND deleted_at IS NULL;

-- name: GetMerchantTimezone :one
SELECT timezone FROM core.merchant WHERE id = $1 AND deleted_at IS NULL;
