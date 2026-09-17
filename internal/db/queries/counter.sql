-- internal/db/queries/counter.sql
-- operations.counter (docs/08-operations-ddl.md §1 "Penjadwalan & Antrian") — no
-- soft-delete columns in this table, "delete" is a PATCH is_active=false instead.

-- name: CreateCounter :one
INSERT INTO operations.counter (company_id, merchant_id, queue_type, department_id, code, name, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetCounterByID :one
SELECT * FROM operations.counter WHERE id = $1;

-- name: ListCountersByMerchant :many
SELECT * FROM operations.counter
WHERE merchant_id = $1
ORDER BY created_at
LIMIT $2 OFFSET $3;

-- name: UpdateCounter :one
UPDATE operations.counter
SET code = $2, name = $3, is_active = $4, updated_by = $5
WHERE id = $1
RETURNING *;
