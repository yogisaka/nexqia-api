-- internal/db/queries/physician_schedule.sql
-- operations.physician_schedule (docs/08-operations-ddl.md §1 "Penjadwalan & Antrian")

-- name: CreatePhysicianSchedule :one
INSERT INTO operations.physician_schedule
    (company_id, merchant_id, physician_id, department_id, day_of_week, start_time, end_time, slot_quota, effective_from, effective_to, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $11)
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
    effective_from = $6, effective_to = $7, is_active = $8, updated_by = $9
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePhysicianSchedule :exec
UPDATE operations.physician_schedule
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
