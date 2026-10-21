-- Identity & packaging queries (plan 2026-10-20-inventory-17b-p3-identity-packaging
-- Task 2, spec 2026-10-20-inventory-17b-procurement-special-cases §6, migration
-- 000082): consignment usage ledger, serial numbers, repacking drafts and the
-- consignment billing report.
--
-- GetConsignmentContractPrice REUSES the P1 query GetEffectiveContractPrice
-- (internal/db/queries/purchase.sql) with kind = 'consignment' — the kind
-- parameter already exists, so no duplicate query is defined here.

-- §6.2 #1 consignment usage (append-only; UPDATE/DELETE revoked).
-- name: InsertConsignmentUsage :one
INSERT INTO inventory.consignment_usage
    (company_id, merchant_id, supplier_id, item_id, lot_id, base_qty,
     unit_price, used_on, ref_type, ref_id, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING id;

-- §6.2 #2 serial numbers.
-- name: InsertSerial :one
INSERT INTO inventory.serial
    (company_id, merchant_id, item_id, serial_no, location_id, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: ListSerials :many
SELECT * FROM inventory.serial
WHERE merchant_id = $1
  AND (sqlc.narg('item_id')::uuid IS NULL OR item_id = sqlc.narg('item_id'))
  AND (sqlc.narg('status')::text IS NULL OR status = sqlc.narg('status'))
ORDER BY item_id, serial_no, id;

-- Fetch serials of one item by their numbers (enforceSerialIssue / moves).
-- name: GetSerialsByIDs :many
SELECT * FROM inventory.serial
WHERE merchant_id = $1 AND item_id = $2 AND serial_no = ANY(sqlc.arg(serial_nos)::text[])
ORDER BY id;

-- name: UpdateSerialIssued :execrows
UPDATE inventory.serial SET
    status = sqlc.arg('status')::text,
    issued_ref_type = sqlc.arg('issued_ref_type')::text,
    issued_ref_id = sqlc.arg('issued_ref_id')::uuid,
    location_id = NULL
WHERE merchant_id = sqlc.arg('merchant_id')::uuid
  AND item_id = sqlc.arg('item_id')::uuid
  AND serial_no = ANY(sqlc.arg('serial_nos')::text[])
  AND status = 'in_stock';

-- Move serials to another merchant/location (status stays in_stock).
-- name: MoveSerialsForTransfer :execrows
UPDATE inventory.serial SET
    merchant_id = sqlc.arg('to_merchant_id')::uuid,
    location_id = sqlc.arg('to_location_id')::uuid
WHERE merchant_id = sqlc.arg('from_merchant_id')::uuid
  AND item_id = sqlc.arg('item_id')::uuid
  AND serial_no = ANY(sqlc.arg('serial_nos')::text[])
  AND status = 'in_stock';

-- §6.2 #3 repacking.
-- name: InsertRepack :one
INSERT INTO inventory.repack
    (company_id, merchant_id, out_location_id, in_location_id, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetRepackByID :one
SELECT * FROM inventory.repack WHERE id = $1;

-- name: ListRepacks :many
SELECT * FROM inventory.repack
WHERE merchant_id = $1
ORDER BY created_at DESC, id DESC;

-- name: UpdateRepackDraft :one
UPDATE inventory.repack r SET
    out_location_id = sqlc.arg('out_location_id')::uuid,
    in_location_id = sqlc.arg('in_location_id')::uuid,
    note = sqlc.narg('note')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE r.id = sqlc.arg('repack_id')::uuid
  AND r.status = 'draft'
  AND r.row_version = sqlc.arg('expected_row_version')::integer
RETURNING r.*;

-- Full line replace for a draft repack (out and in lines together).
-- name: DeleteRepackLines :exec
WITH out_del AS (
    DELETE FROM inventory.repack_out_line AS ol WHERE ol.repack_id = $1 RETURNING 1
), in_del AS (
    DELETE FROM inventory.repack_in_line AS il WHERE il.repack_id = $1 RETURNING 1
)
SELECT 1;

-- name: InsertRepackOutLine :one
INSERT INTO inventory.repack_out_line
    (company_id, merchant_id, repack_id, item_id, lot_id, source, base_qty, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: InsertRepackInLine :one
INSERT INTO inventory.repack_in_line
    (company_id, merchant_id, repack_id, item_id, item_unit_id, pack_qty,
     pack_content, batch_no, expiry_date, base_qty, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: ListRepackOutLines :many
SELECT * FROM inventory.repack_out_line
WHERE repack_id = $1
ORDER BY id;

-- name: ListRepackInLines :many
SELECT * FROM inventory.repack_in_line
WHERE repack_id = $1
ORDER BY id;

-- name: MarkRepackPosted :one
UPDATE inventory.repack SET
    status = 'posted', posted_at = now(),
    posted_by = sqlc.arg('posted_by')::uuid, updated_by = sqlc.arg('posted_by')::uuid
WHERE id = sqlc.arg('repack_id')::uuid AND status = 'draft'
RETURNING *;

-- name: MarkRepackVoid :one
UPDATE inventory.repack SET
    status = 'void', void_reason = sqlc.arg('void_reason')::text,
    updated_by = sqlc.arg('updated_by')::uuid
WHERE id = sqlc.arg('repack_id')::uuid AND status = 'posted'
RETURNING *;

-- §6.2 #1 consignment billing report: aggregate per supplier+item between
-- used_on dates (qty SUM, value SUM(qty × unit_price)), plus per-date details.
-- name: ListConsignmentReport :many
SELECT supplier_id, item_id,
       SUM(base_qty)::numeric AS qty,
       ROUND(SUM(base_qty * unit_price), 6)::numeric AS value
FROM inventory.consignment_usage
WHERE merchant_id = $1
  AND used_on >= $2::date
  AND used_on <= $3::date
  AND (sqlc.narg('supplier_id')::uuid IS NULL OR supplier_id = sqlc.narg('supplier_id'))
GROUP BY supplier_id, item_id
ORDER BY supplier_id, item_id;

-- name: ListConsignmentReportDetails :many
SELECT supplier_id, item_id, used_on, ref_type,
       SUM(base_qty)::numeric AS qty, unit_price
FROM inventory.consignment_usage
WHERE merchant_id = $1
  AND used_on >= $2::date
  AND used_on <= $3::date
  AND (sqlc.narg('supplier_id')::uuid IS NULL OR supplier_id = sqlc.narg('supplier_id'))
GROUP BY supplier_id, item_id, used_on, ref_type, unit_price
ORDER BY supplier_id, item_id, used_on, unit_price;
