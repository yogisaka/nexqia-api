-- Stock ledger queries (plan 2026-10-07-inventory-17a Task 2, spec §4.2–4.5).
-- All inventory.stock_balance / inventory.stock_move writes go through these.

-- name: UpsertMerchantStockSetting :one
INSERT INTO inventory.merchant_setting (merchant_id, company_id)
VALUES ($1, $2)
ON CONFLICT (merchant_id) DO UPDATE SET updated_at = now()
RETURNING stock_locked_until;

-- name: InsertStockBalanceIfAbsent :one
INSERT INTO inventory.stock_balance
    (company_id, merchant_id, location_id, item_id, lot_id, source, status, received_first_at)
VALUES ($1, $2, $3, $4, $5, $6, $7, COALESCE(sqlc.arg(received_at)::timestamptz, now()))
ON CONFLICT (location_id, lot_id, source, status) DO NOTHING
RETURNING id;

-- name: LockStockBalance :one
SELECT id, quantity FROM inventory.stock_balance
WHERE merchant_id = $1 AND location_id = $2 AND lot_id = $3 AND source = $4 AND status = $5
FOR UPDATE;

-- name: AddStockBalanceQuantity :exec
UPDATE inventory.stock_balance SET quantity = quantity + $1, updated_at = now()
WHERE id = $2;

-- name: InsertStockMove :one
INSERT INTO inventory.stock_move
    (company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id,
     source, status, quantity, unit_cost, ref_type, ref_id, reason, override_reason, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16)
RETURNING id;

-- name: LockIssuableStock :many
SELECT b.id, b.lot_id, b.source, b.quantity, b.received_first_at, l.expiry_date
FROM inventory.stock_balance b
JOIN inventory.lot l ON l.id = b.lot_id
WHERE b.merchant_id = $1 AND b.location_id = $2 AND b.item_id = $3
  AND b.status = 'available' AND b.quantity > 0
  AND NOT l.is_recalled
  AND (l.expiry_date IS NULL OR l.expiry_date >= $4)
ORDER BY b.id
FOR UPDATE OF b;

-- name: GetItemAvgCost :one
SELECT CAST(COALESCE(
    (SELECT avg_cost FROM inventory.item_cost WHERE merchant_id = $1 AND item_id = $2),
    0
) AS numeric) AS avg_cost;
