-- internal/db/queries/coldchain.sql
-- Cold chain queries (spec 2026-10-20-inventory-17b-procurement-special-cases
-- §7, migration 000083): opened containers with beyond-use dating (BUD), VVM
-- stage logs, vaccine fridges and their temperature logs. Reads and writes
-- stay inside the caller's request transaction (RLS app.current_merchant_id).

-- name: InsertOpenedContainer :one
INSERT INTO inventory.opened_container
    (company_id, merchant_id, item_id, lot_id, location_id, opened_at,
     reconstituted, bud_at, remaining_base_qty, status, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, 'open', $10, $11)
RETURNING *;

-- name: GetOpenedContainerByID :one
SELECT * FROM inventory.opened_container WHERE id = $1;

-- name: ListOpenedContainers :many
SELECT * FROM inventory.opened_container
WHERE merchant_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status')::text)
ORDER BY bud_at ASC;

-- name: DecrementOpenedContainer :one
UPDATE inventory.opened_container
SET remaining_base_qty = remaining_base_qty - sqlc.arg('qty')::numeric,
    updated_at = now(),
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('id')::uuid
RETURNING remaining_base_qty;

-- name: SetOpenedContainerStatus :one
UPDATE inventory.opened_container
SET status = sqlc.arg('status')::text,
    updated_at = now(),
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('id')::uuid
RETURNING *;

-- name: InsertVvmLog :one
INSERT INTO inventory.vvm_log
    (company_id, merchant_id, opened_container_id, checked_at, stage, checked_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: InsertFridge :one
INSERT INTO inventory.fridge
    (company_id, merchant_id, name, location_id, note, is_active, created_by)
VALUES ($1, $2, $3, sqlc.narg('location_id')::uuid, sqlc.narg('note')::text,
        COALESCE(sqlc.narg('is_active')::boolean, true), $4)
RETURNING *;

-- name: GetFridgeByID :one
SELECT * FROM inventory.fridge WHERE id = $1;

-- name: ListFridges :many
SELECT * FROM inventory.fridge
WHERE merchant_id = $1
ORDER BY name, id;

-- name: UpdateFridge :one
UPDATE inventory.fridge SET
    name = COALESCE(sqlc.narg('name')::text, name),
    location_id = COALESCE(sqlc.narg('location_id')::uuid, location_id),
    note = COALESCE(sqlc.narg('note')::text, note),
    is_active = COALESCE(sqlc.narg('is_active')::boolean, is_active),
    updated_at = now(),
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('id')::uuid
RETURNING *;

-- name: InsertFridgeTempLog :one
INSERT INTO inventory.fridge_temp_log
    (company_id, merchant_id, fridge_id, logged_at, temperature, taken_by, note)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListFridgeTempLogs :many
SELECT * FROM inventory.fridge_temp_log
WHERE merchant_id = $1
  AND (sqlc.narg('from_date')::date IS NULL OR logged_at >= sqlc.narg('from_date')::date)
  AND (sqlc.narg('to_date')::date IS NULL OR logged_at < sqlc.narg('to_date')::date + INTERVAL '1 day')
ORDER BY logged_at DESC;
