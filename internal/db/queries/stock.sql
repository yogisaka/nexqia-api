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
     source, status, quantity, unit_cost, ref_type, ref_id, reason, override_reason,
     reversal_of, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING id;

-- name: FindStockMoveReceipt :one
SELECT id FROM inventory.stock_move
WHERE ref_type = 'goods_receipt' AND ref_id = $1 AND lot_id = $2 AND move_type = 'receipt';

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

-- name: GetMerchantStockSetting :one
SELECT min_shelf_life_months, min_shelf_life_vaccine_months,
       input_vat_creditable, default_vat_percent
FROM inventory.merchant_setting
WHERE merchant_id = $1;

-- ApplyReceiptMovingAverage updates the moving average cost (spec §4.1.5) in
-- SQL numeric: qty_basis = total purchase balances BEFORE this receipt; the
-- whole computation and the item_cost UPSERT run inside Postgres.
-- Params: company, merchant, item, sqlc.arg('unit_cost')::numeric AS unit_cost, sqlc.arg('base_qty')::numeric AS base_qty.
-- name: ApplyReceiptMovingAverage :one
WITH basis AS (
    SELECT COALESCE(SUM(b.quantity), 0)::numeric AS qty
    FROM inventory.stock_balance b
    WHERE b.merchant_id = $2 AND b.item_id = $3 AND b.source = 'purchase'
), cost AS (
    SELECT COALESCE(
        (SELECT avg_cost FROM inventory.item_cost WHERE merchant_id = $2 AND item_id = $3),
        0)::numeric AS avg
), next AS (
    SELECT CASE WHEN basis.qty + sqlc.arg('base_qty')::numeric = 0 THEN cost.avg
                ELSE ROUND((basis.qty * cost.avg + sqlc.arg('base_qty')::numeric * sqlc.arg('unit_cost')::numeric)
                           / (basis.qty + sqlc.arg('base_qty')::numeric), 6) END AS avg
    FROM basis, cost
)
INSERT INTO inventory.item_cost (company_id, merchant_id, item_id, avg_cost)
SELECT $1, $2, $3, next.avg FROM next
ON CONFLICT (merchant_id, item_id)
DO UPDATE SET avg_cost = EXCLUDED.avg_cost, updated_at = now()
RETURNING avg_cost;

-- ReverseReceiptMovingAverage rolls the average back for a voided receipt
-- (spec §4.1.7): (qty_basis × avg − base_qty × unit_cost) ÷ (qty_basis −
-- base_qty); when the remaining basis reaches 0 the average stays.
-- Params: company, merchant, item, sqlc.arg('unit_cost')::numeric AS unit_cost, sqlc.arg('base_qty')::numeric AS base_qty.
-- name: ReverseReceiptMovingAverage :one
WITH basis AS (
    SELECT COALESCE(SUM(b.quantity), 0)::numeric AS qty
    FROM inventory.stock_balance b
    WHERE b.merchant_id = $2 AND b.item_id = $3 AND b.source = 'purchase'
), cost AS (
    SELECT COALESCE(
        (SELECT avg_cost FROM inventory.item_cost WHERE merchant_id = $2 AND item_id = $3),
        0)::numeric AS avg
), next AS (
    SELECT CASE WHEN basis.qty - sqlc.arg('base_qty')::numeric <= 0 THEN cost.avg
                ELSE ROUND((basis.qty * cost.avg - sqlc.arg('base_qty')::numeric * sqlc.arg('unit_cost')::numeric)
                           / (basis.qty - sqlc.arg('base_qty')::numeric), 6) END AS avg
    FROM basis, cost
)
INSERT INTO inventory.item_cost (company_id, merchant_id, item_id, avg_cost)
SELECT $1, $2, $3, next.avg FROM next
ON CONFLICT (merchant_id, item_id)
DO UPDATE SET avg_cost = EXCLUDED.avg_cost, updated_at = now()
RETURNING avg_cost;

-- §4.1 suppliers (core.supplier, migration 000010 + 000071): company-level
-- (merchant_id NULL) next to merchant-only ones.
-- name: ListSuppliers :many
SELECT * FROM core.supplier
WHERE company_id = $1 AND deleted_at IS NULL
  AND (merchant_id IS NULL OR merchant_id = $2)
ORDER BY code, id;

-- name: GetSupplierByID :one
SELECT * FROM core.supplier WHERE id = $1 AND deleted_at IS NULL;

-- name: InsertSupplier :one
INSERT INTO core.supplier (
    company_id, merchant_id, code, name, pic_name, pic_phone, address, npwp,
    bank_account, credit_limit, lead_time_days, lead_time_cito_days,
    payment_term_days, contract_start_date, contract_end_date, is_active, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8,
    $9, $10, $11, $12,
    $13, $14, $15, $16, $17
) RETURNING *;

-- name: UpdateSupplier :one
UPDATE core.supplier s SET
    name = COALESCE(sqlc.narg('name')::text, s.name),
    pic_name = COALESCE(sqlc.narg('pic_name')::text, s.pic_name),
    pic_phone = COALESCE(sqlc.narg('pic_phone')::text, s.pic_phone),
    address = COALESCE(sqlc.narg('address')::text, s.address),
    npwp = COALESCE(sqlc.narg('npwp')::text, s.npwp),
    bank_account = COALESCE(sqlc.narg('bank_account')::jsonb, s.bank_account),
    credit_limit = COALESCE(sqlc.narg('credit_limit')::numeric, s.credit_limit),
    lead_time_days = COALESCE(sqlc.narg('lead_time_days')::integer, s.lead_time_days),
    lead_time_cito_days = COALESCE(sqlc.narg('lead_time_cito_days')::integer, s.lead_time_cito_days),
    payment_term_days = COALESCE(sqlc.narg('payment_term_days')::integer, s.payment_term_days),
    contract_start_date = COALESCE(sqlc.narg('contract_start_date')::date, s.contract_start_date),
    contract_end_date = COALESCE(sqlc.narg('contract_end_date')::date, s.contract_end_date),
    is_active = COALESCE(sqlc.narg('is_active')::boolean, s.is_active),
    updated_by = sqlc.arg('updated_by')::uuid
WHERE s.id = sqlc.arg('supplier_id')::uuid
  AND s.row_version = sqlc.arg('expected_row_version')::integer
  AND s.deleted_at IS NULL
RETURNING s.*;

-- name: SoftDeleteSupplier :execrows
UPDATE core.supplier SET deleted_at = now(), deleted_by = sqlc.arg('deleted_by')::uuid,
    updated_by = sqlc.arg('deleted_by')::uuid
WHERE id = sqlc.arg('supplier_id')::uuid AND deleted_at IS NULL;

-- §4.1 goods receipts.
-- name: InsertGoodsReceipt :one
INSERT INTO inventory.goods_receipt (
    company_id, merchant_id, supplier_id, location_id, source, invoice_no,
    invoice_date, received_on, invoice_discount, freight, vat_included,
    vat_percent, note, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
) RETURNING *;

-- name: GetGoodsReceiptByID :one
SELECT * FROM inventory.goods_receipt WHERE id = $1;

-- name: ListGoodsReceipts :many
SELECT * FROM inventory.goods_receipt
WHERE merchant_id = $1
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
  AND (sqlc.narg('from_date')::date IS NULL OR received_on >= sqlc.narg('from_date'))
  AND (sqlc.narg('to_date')::date IS NULL OR received_on <= sqlc.narg('to_date'))
ORDER BY received_on DESC, id DESC;

-- name: UpdateGoodsReceiptDraft :one
UPDATE inventory.goods_receipt g SET
    supplier_id = sqlc.narg('supplier_id')::uuid,
    location_id = sqlc.arg('location_id')::uuid,
    source = sqlc.arg('source')::text,
    invoice_no = sqlc.narg('invoice_no')::text,
    invoice_date = sqlc.narg('invoice_date')::date,
    received_on = sqlc.arg('received_on')::date,
    invoice_discount = sqlc.arg('invoice_discount')::numeric,
    freight = sqlc.arg('freight')::numeric,
    vat_included = sqlc.arg('vat_included')::boolean,
    vat_percent = sqlc.arg('vat_percent')::numeric,
    note = sqlc.narg('note')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE g.id = sqlc.arg('receipt_id')::uuid
  AND g.status = 'draft'
  AND g.row_version = sqlc.arg('expected_row_version')::integer
RETURNING g.*;

-- name: DeleteGoodsReceiptLines :exec
DELETE FROM inventory.goods_receipt_line WHERE receipt_id = $1;

-- name: InsertGoodsReceiptLine :one
INSERT INTO inventory.goods_receipt_line (
    company_id, merchant_id, receipt_id, item_id, item_unit_id, pack_qty,
    pack_content, bonus_pack_qty, pack_price, line_discount_percent,
    batch_no, expiry_date, short_shelf_life_reason, sort_order
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14
) RETURNING *;

-- name: ListGoodsReceiptLines :many
SELECT * FROM inventory.goods_receipt_line
WHERE receipt_id = $1
ORDER BY sort_order, id;

-- name: CompleteGoodsReceiptLine :one
UPDATE inventory.goods_receipt_line SET
    base_qty = sqlc.arg('base_qty')::numeric,
    unit_cost = sqlc.arg('unit_cost')::numeric,
    hna_ppn_per_pack = sqlc.arg('hna_ppn_per_pack')::numeric,
    lot_id = sqlc.arg('lot_id')::uuid
WHERE id = sqlc.arg('line_id')::uuid
RETURNING *;

-- name: MarkGoodsReceiptPosted :one
UPDATE inventory.goods_receipt SET
    status = 'posted', posted_at = now(), posted_by = sqlc.arg('posted_by')::uuid,
    updated_by = sqlc.arg('posted_by')::uuid
WHERE id = sqlc.arg('receipt_id')::uuid AND status = 'draft'
RETURNING *;

-- name: MarkGoodsReceiptVoid :one
UPDATE inventory.goods_receipt SET
    status = 'void', void_reason = sqlc.arg('void_reason')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('receipt_id')::uuid AND status = 'posted'
RETURNING *;

-- name: UpsertReceiptLot :one
INSERT INTO inventory.lot (company_id, item_id, batch_no, expiry_date)
VALUES ($1, $2, $3, $4)
ON CONFLICT (item_id, batch_no, COALESCE(expiry_date, DATE '9999-12-31'))
DO UPDATE SET batch_no = EXCLUDED.batch_no
RETURNING *;
