-- internal/db/queries/queue.sql
-- operations.queue + operations.queue_status_history (docs/08-operations-ddl.md §1).
-- One generic table for all stages (queue_type discriminator) — see
-- 2026-09-16-v1-operations-antrian-jadwal-design.md §4.

-- name: CreateQueue :one
INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetQueueByID :one
SELECT * FROM operations.queue WHERE id = $1;

-- name: ListQueueForWork :many
SELECT * FROM operations.queue
WHERE merchant_id = $1 AND queue_type = $2 AND status = $3
ORDER BY created_at
LIMIT $4 OFFSET $5;

-- name: ListActiveQueueForDisplay :many
SELECT * FROM operations.queue
WHERE merchant_id = $1
  AND (sqlc.narg('department_id')::uuid IS NULL OR department_id = sqlc.narg('department_id'))
  AND status IN ('waiting','called','in_progress')
ORDER BY queue_type, created_at
LIMIT 100;

-- name: GetOldestWaitingQueue :one
SELECT * FROM operations.queue
WHERE merchant_id = $1 AND queue_type = $2
  AND (sqlc.narg('department_id')::uuid IS NULL OR department_id = sqlc.narg('department_id'))
  AND status = 'waiting'
ORDER BY created_at
LIMIT 1;

-- name: CountTodayQueueByType :one
SELECT count(*) FROM operations.queue
WHERE merchant_id = $1 AND queue_type = $2 AND department_id = $3 AND created_at::date = current_date;

-- name: UpdateQueueStatus :one
UPDATE operations.queue
SET status = $2, counter_id = $3, called_at = $4, updated_by = $5
WHERE id = $1
RETURNING *;

-- name: CreateQueueStatusHistory :exec
INSERT INTO operations.queue_status_history (queue_id, counter_id, from_status, to_status, changed_by)
VALUES ($1, $2, $3, $4, $5);
