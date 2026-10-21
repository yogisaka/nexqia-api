-- internal/db/queries/emergency.sql
-- Emergency kit queries (spec 2026-10-20-inventory-17b-procurement-special-
-- cases §7, migration 000083): sealed emergency kits (troli emergensi), their
-- standard contents, single-active seals and the report's available-balance /
-- nearest-expiry reads. Writes stay inside the caller's request transaction
-- (RLS app.current_merchant_id); balance decrements go through the locked
-- ledger (issueStock), never through this file.

-- name: InsertEmergencyKit :one
INSERT INTO inventory.emergency_kit
    (company_id, merchant_id, name, location_id, note, is_active, created_by)
VALUES ($1, $2, $3, $4, sqlc.narg('note')::text,
        COALESCE(sqlc.narg('is_active')::boolean, true), $5)
RETURNING *;

-- name: GetEmergencyKitByID :one
SELECT * FROM inventory.emergency_kit WHERE id = $1;

-- name: ListEmergencyKits :many
SELECT * FROM inventory.emergency_kit
WHERE merchant_id = $1
ORDER BY name, id;

-- name: UpdateEmergencyKit :one
UPDATE inventory.emergency_kit SET
    name = COALESCE(sqlc.narg('name')::text, name),
    note = COALESCE(sqlc.narg('note')::text, note),
    is_active = COALESCE(sqlc.narg('is_active')::boolean, is_active),
    updated_at = now(),
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('id')::uuid
RETURNING *;

-- name: DeleteEmergencyKitItems :exec
DELETE FROM inventory.emergency_kit_item WHERE kit_id = $1;

-- name: InsertEmergencyKitItem :one
INSERT INTO inventory.emergency_kit_item
    (company_id, merchant_id, kit_id, item_id, base_qty, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListEmergencyKitItems :many
SELECT ki.*, i.name AS item_name
FROM inventory.emergency_kit_item ki
JOIN inventory.item i ON i.id = ki.item_id
WHERE ki.kit_id = $1
ORDER BY i.name, ki.id;

-- name: InsertEmergencyKitSeal :one
INSERT INTO inventory.emergency_kit_seal
    (company_id, merchant_id, kit_id, seal_no, sealed_at, sealed_by, note)
VALUES ($1, $2, $3, $4, $5, $6, sqlc.narg('note')::text)
RETURNING *;

-- name: GetActiveSealByKit :one
SELECT * FROM inventory.emergency_kit_seal
WHERE kit_id = $1 AND broken_at IS NULL;

-- name: GetEmergencyKitSealByID :one
SELECT * FROM inventory.emergency_kit_seal WHERE id = $1;

-- name: BreakEmergencyKitSeal :one
UPDATE inventory.emergency_kit_seal
SET broken_at = $2, broken_by = $3
WHERE id = $1
RETURNING *;

-- SumAvailableByItemLocation — the available saldo of one item at one
-- location (the refill check and the report's on_hand).
-- name: SumAvailableByItemLocation :one
SELECT COALESCE(SUM(quantity), 0)::numeric AS available
FROM inventory.stock_balance
WHERE merchant_id = $1 AND location_id = $2 AND item_id = $3
  AND status = 'available';

-- ListKitItemExpiry — nearest expiry date per item, taken from the available
-- saldo with quantity > 0 at the kit location (report column nearest_expiry).
-- name: ListKitItemExpiry :many
SELECT b.item_id, MIN(l.expiry_date) AS nearest_expiry
FROM inventory.stock_balance b
JOIN inventory.lot l ON l.id = b.lot_id
WHERE b.merchant_id = $1 AND b.location_id = $2
  AND b.status = 'available' AND b.quantity > 0
  AND l.expiry_date IS NOT NULL
  AND b.item_id = ANY($3::uuid[])
GROUP BY b.item_id;
