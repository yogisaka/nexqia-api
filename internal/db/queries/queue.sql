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
-- SKIP LOCKED: two stage-less counters calling at once never pull the same ticket (pattern of CallNextFlowTicket).
SELECT * FROM operations.queue
WHERE merchant_id = $1 AND queue_type = $2
  AND (sqlc.narg('department_id')::uuid IS NULL OR department_id = sqlc.narg('department_id'))
  AND status = 'waiting'
ORDER BY created_at
LIMIT 1
FOR UPDATE SKIP LOCKED;

-- name: CountQueueByTypeSince :one
-- Legacy (journey-less) ticket sequence; since = the merchant's local
-- midnight (spec 2026-10-06-daily-numbering §3.3).
SELECT count(*) FROM operations.queue
WHERE merchant_id = $1 AND queue_type = $2 AND department_id = $3 AND created_at >= sqlc.arg('since')::timestamptz;

-- name: UpdateQueueStatus :one
UPDATE operations.queue
SET status = $2, counter_id = $3, called_at = $4, updated_by = $5
WHERE id = $1
RETURNING *;

-- name: CreateQueueStatusHistory :exec
INSERT INTO operations.queue_status_history (queue_id, counter_id, from_status, to_status, changed_by)
VALUES ($1, $2, $3, $4, $5);

-- name: FinishQueueTicket :one
UPDATE operations.queue
SET status = $2, finished_at = now(), updated_by = $3
WHERE id = $1
RETURNING *;

-- name: StartQueueTicket :one
UPDATE operations.queue
SET status = 'in_progress', started_at = now(), served_by = $2, updated_by = $2
WHERE id = $1
RETURNING *;

-- name: UpdateQueuePriority :one
UPDATE operations.queue
SET priority = $2, updated_by = $3
WHERE id = $1
RETURNING *;

-- name: CallNextFlowTicket :one
SELECT * FROM operations.queue
WHERE stage_id = $1 AND status = 'waiting'
  AND (counter_id = $2::uuid OR counter_id IS NULL)
ORDER BY priority DESC, checked_in_at, created_at
LIMIT 1
FOR UPDATE SKIP LOCKED;
