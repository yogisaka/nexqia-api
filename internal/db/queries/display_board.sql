-- internal/db/queries/display_board.sql
-- Token-protected display boards (spec 2026-10-01-c-queue-flow-display §5–§6).
-- The raw token never touches the database: only sha256(token) hex is stored.

-- name: ListDisplayBoardsByMerchant :many
SELECT * FROM operations.display_board
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: GetDisplayBoardByID :one
SELECT * FROM operations.display_board WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateDisplayBoard :one
INSERT INTO operations.display_board (company_id, merchant_id, name, location_id, stage_ids, counter_ids, layout, show_next_n, voice, name_display, poll_seconds, is_active, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: UpdateDisplayBoard :one
UPDATE operations.display_board
SET name = $2, location_id = $3, stage_ids = $4, counter_ids = $5, layout = $6,
    show_next_n = $7, voice = $8, name_display = $9, poll_seconds = $10,
    is_active = $11, updated_by = $12
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteDisplayBoard :exec
UPDATE operations.display_board
SET deleted_at = now(), deleted_by = $2, updated_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: SetDisplayBoardToken :exec
UPDATE operations.display_board
SET token_hash = $2, token_created_at = now(), updated_by = $3
WHERE id = $1 AND deleted_at IS NULL;

-- name: ClearDisplayBoardToken :exec
UPDATE operations.display_board
SET token_hash = NULL, token_created_at = NULL, updated_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListBoardFeedTickets :many
-- Tickets visible on a board: called/in_progress (current) plus waiting, for
-- the board's stage_ids/counter_ids. Deliberately selects NO patient
-- identifiers — only the full_name, which the handler masks per
-- display_board.name_display before responding (spec §6).
SELECT q.queue_number, q.status, q.called_at, q.priority,
       st.name AS stage_name, st.kind AS stage_kind,
       c.name AS counter_name, c.code AS counter_code,
       per.full_name AS patient_name,
       phy.full_name AS physician_name
FROM operations.queue q
LEFT JOIN operations.queue_stage st ON st.id = q.stage_id
LEFT JOIN operations.counter c ON c.id = q.counter_id
JOIN core.person per ON per.id = q.person_id
JOIN operations.admission ad ON ad.id = q.admission_id
LEFT JOIN core.person phy ON phy.id = ad.physician_id
WHERE q.merchant_id = $1
  AND q.status IN ('called','in_progress','waiting')
  AND (q.stage_id = ANY($2::uuid[]) OR q.counter_id = ANY($3::uuid[]))
ORDER BY q.called_at DESC NULLS LAST, q.created_at;
