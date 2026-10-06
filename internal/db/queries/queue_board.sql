-- internal/db/queries/queue_board.sql
-- operations.queue_board (migrations/000022_operations_queue_board_view.up.sql) —
-- read-only joined view for staff queue board/dashboard screens.

-- name: ListQueueBoard :many
SELECT * FROM operations.queue_board
WHERE merchant_id = $1
  AND queue_type = $2
  AND (sqlc.narg('date')::date IS NULL OR (created_at AT TIME ZONE sqlc.arg('tz')::text)::date = sqlc.narg('date')::date)
  AND (sqlc.narg('statuses')::text[] IS NULL OR status = ANY(sqlc.narg('statuses')::text[]))
  AND (
    sqlc.narg('search')::text IS NULL
    OR person_full_name ILIKE '%' || sqlc.narg('search')::text || '%'
    OR person_medical_record_no ILIKE '%' || sqlc.narg('search')::text || '%'
  )
ORDER BY created_at
LIMIT $3 OFFSET $4;
