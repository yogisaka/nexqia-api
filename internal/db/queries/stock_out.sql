-- Stock outbound value flows (plan 2026-10-20-inventory-17b-p2 Task 2, spec
-- 2026-10-20-inventory-17b-procurement-special-cases §5, migration 000081):
-- supplier returns, destructions and inter-merchant transfers. All balance
-- writes still go through applyStockMove (stock_ledger.go); these queries only
-- touch the P2 document tables and lock allocation candidates.

-- §5.2 #1 supplier returns.

-- name: InsertSupplierReturn :one
INSERT INTO inventory.supplier_return (
    company_id, merchant_id, supplier_id, return_no, reason, note, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetSupplierReturnByID :one
SELECT * FROM inventory.supplier_return WHERE id = $1;

-- name: ListSupplierReturns :many
SELECT * FROM inventory.supplier_return
WHERE merchant_id = $1
ORDER BY created_at DESC, id DESC;

-- name: UpdateSupplierReturnDraft :one
UPDATE inventory.supplier_return SET
    supplier_id = sqlc.arg('supplier_id')::uuid,
    reason = sqlc.arg('reason')::text,
    note = sqlc.narg('note')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('return_id')::uuid
  AND status = 'draft'
  AND row_version = sqlc.arg('expected_row_version')::integer
RETURNING *;

-- name: DeleteSupplierReturnLines :exec
DELETE FROM inventory.supplier_return_line WHERE return_id = $1;

-- name: InsertSupplierReturnLine :one
INSERT INTO inventory.supplier_return_line (
    company_id, merchant_id, return_id, item_id, lot_id, source,
    base_qty, unit_cost, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListSupplierReturnLines :many
SELECT * FROM inventory.supplier_return_line
WHERE return_id = $1
ORDER BY created_at, id;

-- name: MarkSupplierReturnPosted :one
UPDATE inventory.supplier_return SET
    status = 'posted', posted_at = now(), posted_by = sqlc.arg('posted_by')::uuid,
    updated_by = sqlc.arg('posted_by')::uuid
WHERE id = sqlc.arg('return_id')::uuid AND status = 'draft'
RETURNING *;

-- name: MarkSupplierReturnVoid :one
UPDATE inventory.supplier_return SET
    status = 'void', void_reason = sqlc.arg('void_reason')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('return_id')::uuid AND status = 'posted'
RETURNING *;

-- name: CountSupplierReturnsSince :one
SELECT COUNT(*) FROM inventory.supplier_return
WHERE merchant_id = $1 AND created_at >= $2;

-- Prefill for a return line: the unit cost of the latest receipt move of the
-- lot (spec §5.2 #1). 0 when the lot was never received.
-- name: LastReceiptUnitCost :one
SELECT unit_cost FROM inventory.stock_move
WHERE lot_id = $1 AND move_type = 'receipt'
ORDER BY created_at DESC, id DESC
LIMIT 1;

-- Return allocation candidates: every available balance row of the merchant
-- for one lot+source, oldest receipt first (spec §5.2 #1 — returns have no
-- location, stock is drawn across locations). All rows are locked.
-- name: LockAvailableByLot :many
SELECT id, location_id, quantity, received_first_at
FROM inventory.stock_balance
WHERE merchant_id = $1 AND lot_id = $2 AND source = $3 AND status = 'available'
ORDER BY received_first_at, id
FOR UPDATE;

-- §5.2 #2 destructions.

-- name: InsertDestruction :one
INSERT INTO inventory.destruction (
    company_id, merchant_id, location_id, occurred_on, destruction_no,
    method, witnesses, note, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetDestructionByID :one
SELECT * FROM inventory.destruction WHERE id = $1;

-- name: ListDestructions :many
SELECT * FROM inventory.destruction
WHERE merchant_id = $1
ORDER BY created_at DESC, id DESC;

-- name: UpdateDestructionDraft :one
UPDATE inventory.destruction SET
    location_id = sqlc.arg('location_id')::uuid,
    occurred_on = sqlc.arg('occurred_on')::date,
    method = sqlc.arg('method')::text,
    witnesses = sqlc.arg('witnesses')::jsonb,
    note = sqlc.narg('note')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('destruction_id')::uuid
  AND status = 'draft'
  AND row_version = sqlc.arg('expected_row_version')::integer
RETURNING *;

-- name: DeleteDestructionLines :exec
DELETE FROM inventory.destruction_line WHERE destruction_id = $1;

-- name: InsertDestructionLine :one
INSERT INTO inventory.destruction_line (
    company_id, merchant_id, destruction_id, item_id, lot_id, source,
    from_status, base_qty, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListDestructionLines :many
SELECT * FROM inventory.destruction_line
WHERE destruction_id = $1
ORDER BY created_at, id;

-- name: MarkDestructionPosted :one
UPDATE inventory.destruction SET
    status = 'posted', posted_at = now(), posted_by = sqlc.arg('posted_by')::uuid,
    updated_by = sqlc.arg('posted_by')::uuid
WHERE id = sqlc.arg('destruction_id')::uuid AND status = 'draft'
RETURNING *;

-- name: MarkDestructionVoid :one
UPDATE inventory.destruction SET
    status = 'void', void_reason = sqlc.arg('void_reason')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('destruction_id')::uuid AND status = 'posted'
RETURNING *;

-- name: CountDestructionsSince :one
SELECT COUNT(*) FROM inventory.destruction
WHERE merchant_id = $1 AND created_at >= $2;

-- §5.2 #4 inter-merchant transfers (company-level documents).

-- name: InsertInterMerchantTransfer :one
INSERT INTO inventory.inter_merchant_transfer (
    company_id, from_merchant_id, to_merchant_id, from_location_id,
    to_location_id, imt_no, note, sent_by, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
RETURNING *;

-- name: GetInterMerchantTransferByID :one
SELECT * FROM inventory.inter_merchant_transfer WHERE id = $1;

-- name: ListInterMerchantTransfers :many
SELECT * FROM inventory.inter_merchant_transfer
WHERE company_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
ORDER BY sent_at DESC, id DESC;

-- name: InsertInterMerchantTransferLine :one
INSERT INTO inventory.inter_merchant_transfer_line (
    company_id, inter_transfer_id, item_id, lot_id, source, status,
    base_qty, unit_cost, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListInterMerchantTransferLines :many
SELECT * FROM inventory.inter_merchant_transfer_line
WHERE inter_transfer_id = $1
ORDER BY created_at, id;

-- name: MarkInterMerchantTransferReceived :one
UPDATE inventory.inter_merchant_transfer SET
    status = 'received', received_at = now(),
    received_by = sqlc.arg('received_by')::uuid,
    received_on = sqlc.arg('received_on')::date,
    updated_by = sqlc.arg('received_by')::uuid
WHERE id = sqlc.arg('transfer_id')::uuid AND status = 'sent'
RETURNING *;

-- name: MarkInterMerchantTransferCancelled :one
UPDATE inventory.inter_merchant_transfer SET
    status = 'cancelled', updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('transfer_id')::uuid AND status = 'sent'
RETURNING *;

-- name: CountInterMerchantTransfersSince :one
SELECT COUNT(*) FROM inventory.inter_merchant_transfer
WHERE company_id = $1 AND created_at >= $2;
