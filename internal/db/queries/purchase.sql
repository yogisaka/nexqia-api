-- Procurement queries (plan 2026-10-20-inventory-17b-p1-procurement-core
-- Task 2, spec 2026-10-20-inventory-17b-procurement-special-cases §4.2–4.3,
-- migration 000080_procurement_core): procurement settings, central item
-- list, contract prices, purchase request and purchase order lifecycle.

-- §4.1 procurement settings + central items.

-- name: GetProcurementSetting :one
SELECT * FROM inventory.procurement_setting WHERE company_id = $1;

-- name: UpsertProcurementSetting :one
INSERT INTO inventory.procurement_setting
    (company_id, mode, headquarters_merchant_id, updated_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (company_id) DO UPDATE SET
    mode = EXCLUDED.mode,
    headquarters_merchant_id = EXCLUDED.headquarters_merchant_id,
    updated_at = now(),
    updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: InsertProcurementCentralItem :one
INSERT INTO inventory.procurement_central_item (company_id, item_id, created_by)
VALUES ($1, $2, $3)
RETURNING *;

-- name: DeleteProcurementCentralItem :exec
DELETE FROM inventory.procurement_central_item WHERE company_id = $1;

-- name: ListProcurementCentralItems :many
SELECT * FROM inventory.procurement_central_item
WHERE company_id = $1
ORDER BY created_at, id;

-- name: IsProcurementCentralItem :one
SELECT EXISTS (
    SELECT 1 FROM inventory.procurement_central_item
    WHERE company_id = $1 AND item_id = $2
) AS is_central;

-- §4.1 contract prices.

-- name: InsertContractPrice :one
INSERT INTO inventory.contract_price (
    company_id, supplier_id, item_id, item_unit_id, kind,
    pack_price, valid_from, valid_until, note, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10
)
RETURNING *;

-- name: ListContractPrices :many
SELECT * FROM inventory.contract_price
WHERE company_id = $1
ORDER BY supplier_id, item_id, item_unit_id, kind, valid_from DESC;

-- name: DeleteContractPrice :exec
DELETE FROM inventory.contract_price WHERE id = $1 AND company_id = $2;

-- GetEffectiveContractPrice: the price in force on on_date — newest
-- valid_from that started on/before it and has not ended before it.
-- name: GetEffectiveContractPrice :one
SELECT * FROM inventory.contract_price
WHERE company_id = $1 AND supplier_id = $2 AND item_id = $3
  AND item_unit_id = $4 AND kind = $5
  AND valid_from <= $6::date
  AND (valid_until IS NULL OR valid_until >= $6::date)
ORDER BY valid_from DESC
LIMIT 1;

-- §4.1 purchase requests.

-- name: InsertPurchaseRequest :one
INSERT INTO inventory.purchase_request (
    company_id, merchant_id, location_id, pr_no, needed_on, note, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetPurchaseRequestByID :one
SELECT * FROM inventory.purchase_request WHERE id = $1;

-- name: ListPurchaseRequests :many
SELECT * FROM inventory.purchase_request
WHERE merchant_id = $1
ORDER BY created_at DESC;

-- name: UpdatePurchaseRequestDraft :one
UPDATE inventory.purchase_request SET
    location_id = $2,
    needed_on = $3,
    note = $4,
    updated_by = $5
WHERE id = $1 AND row_version = $6
RETURNING *;

-- name: DeletePurchaseRequestLines :exec
DELETE FROM inventory.purchase_request_line WHERE request_id = $1;

-- name: InsertPurchaseRequestLine :one
INSERT INTO inventory.purchase_request_line (
    company_id, merchant_id, request_id, item_id, item_unit_id, pack_qty, note, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListPurchaseRequestLines :many
SELECT * FROM inventory.purchase_request_line
WHERE request_id = $1
ORDER BY created_at, id;

-- name: MarkPurchaseRequestSubmitted :one
UPDATE inventory.purchase_request SET
    status = 'submitted', submitted_at = now(), submitted_by = $2, updated_by = $2
WHERE id = $1 AND status = 'draft'
RETURNING *;

-- MarkPurchaseRequestDecided covers approve (rejection_reason NULL), reject
-- (rejection_reason required by chk_pr_rejection) and convert.
-- name: MarkPurchaseRequestDecided :one
UPDATE inventory.purchase_request SET
    status = sqlc.arg('new_status'), decided_at = now(), decided_by = sqlc.arg('decided_by'),
    rejection_reason = COALESCE(sqlc.narg('rejection_reason'), rejection_reason),
    updated_by = sqlc.arg('decided_by')
WHERE id = sqlc.arg('request_id') AND status = sqlc.arg('from_status')
RETURNING *;

-- name: MarkPurchaseRequestCancelled :one
UPDATE inventory.purchase_request SET
    status = 'cancelled', decided_at = now(), decided_by = $2, updated_by = $2
WHERE id = $1 AND status IN ('draft', 'submitted')
RETURNING *;

-- name: CountPurchaseRequestsSince :one
SELECT COUNT(*) FROM inventory.purchase_request
WHERE merchant_id = $1 AND created_at >= $2;

-- §4.1 purchase orders.

-- name: InsertPurchaseOrder :one
INSERT INTO inventory.purchase_order (
    company_id, merchant_id, supplier_id, location_id, request_id, po_no, note, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetPurchaseOrderByID :one
SELECT * FROM inventory.purchase_order WHERE id = $1;

-- name: ListPurchaseOrders :many
SELECT * FROM inventory.purchase_order
WHERE merchant_id = $1
ORDER BY created_at DESC;

-- name: UpdatePurchaseOrderDraft :one
UPDATE inventory.purchase_order SET
    supplier_id = $2,
    location_id = $3,
    note = $4,
    updated_by = $5
WHERE id = $1 AND row_version = $6
RETURNING *;

-- name: DeletePurchaseOrderLines :exec
DELETE FROM inventory.purchase_order_line WHERE order_id = $1;

-- name: InsertPurchaseOrderLine :one
INSERT INTO inventory.purchase_order_line (
    company_id, merchant_id, order_id, item_id, item_unit_id,
    pack_qty, pack_content, pack_price, note, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: ListPurchaseOrderLines :many
SELECT * FROM inventory.purchase_order_line
WHERE order_id = $1
ORDER BY created_at, id;

-- name: MarkPurchaseOrderSent :one
UPDATE inventory.purchase_order SET
    status = 'sent', sent_at = now(), sent_by = $2, updated_by = $2
WHERE id = $1 AND status = 'draft'
RETURNING *;

-- name: MarkPurchaseOrderClosed :one
UPDATE inventory.purchase_order SET
    status = 'closed', closed_at = now(), updated_by = $2
WHERE id = $1 AND status IN ('sent', 'partial')
RETURNING *;

-- name: MarkPurchaseOrderCancelled :one
UPDATE inventory.purchase_order po SET
    status = 'cancelled', cancel_reason = $2, updated_by = $3
WHERE po.id = $1
  AND (po.status = 'draft'
       OR (po.status = 'sent' AND NOT EXISTS (
           SELECT 1 FROM inventory.purchase_order_line pol
           WHERE pol.order_id = po.id AND pol.received_base_qty > 0)))
RETURNING *;

-- name: AddPurchaseOrderLineReceived :exec
UPDATE inventory.purchase_order_line
SET received_base_qty = received_base_qty + $1
WHERE id = $2;

-- name: SubPurchaseOrderLineReceived :exec
UPDATE inventory.purchase_order_line
SET received_base_qty = received_base_qty - $1
WHERE id = $2;

-- name: SetPurchaseOrderStatus :one
UPDATE inventory.purchase_order SET status = $2, updated_by = $3
WHERE id = $1
RETURNING *;

-- PO daily numbering mirror of CountPurchaseRequestsSince (KEPUTUSAN).
-- name: CountPurchaseOrdersSince :one
SELECT COUNT(*) FROM inventory.purchase_order
WHERE merchant_id = $1 AND created_at >= $2;
