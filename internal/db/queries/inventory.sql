-- internal/db/queries/inventory.sql
-- inventory.unit dictionary, company-level item master, packaging and
-- ingredients (spec 2026-10-07-tariff-c-drug-master-pricing §3.2-§3.5).
-- Multi-table queries qualify every column with its table alias (lesson from
-- ReopenPreviousPriceVersion: sqlc rejects the ambiguous unqualified form).

-- name: ListInventoryUnits :many
SELECT u.id, u.code, u.name, u.ucum_code
FROM inventory.unit u
ORDER BY u.sort_order;

-- name: GetInventoryUnitByID :one
SELECT u.* FROM inventory.unit u WHERE u.id = $1;

-- name: CreateInventoryItem :one
INSERT INTO inventory.item (
    company_id, code, name, item_kind, pricing_category, base_unit_id,
    kfa_code, kfa_pov_code, non_kfa, non_kfa_reason, nie, manufacturer,
    dosage_form, drug_class, is_oot, is_fornas, fornas_period, is_high_alert,
    is_lasa, lasa_note, is_generic, het_amount, het_effective_from,
    min_fraction, no_crush, bud_after_open_hours,
    bud_after_reconstitution_hours, is_single_dose, storage_temp,
    track_batch, track_expiry, is_active, created_by, updated_by
) VALUES (
    $1, $2, $3, $4, $5, $6,
    $7, $8, $9, $10, $11, $12,
    $13, $14, $15, $16, $17, $18,
    $19, $20, $21, $22, $23,
    $24, $25, $26,
    $27, $28, $29,
    $30, $31, $32, $33, $33
)
RETURNING *;

-- name: GetInventoryItemByID :one
SELECT i.* FROM inventory.item i WHERE i.id = $1 AND i.deleted_at IS NULL;

-- name: ListInventoryItems :many
SELECT i.id, i.code, i.name, i.item_kind, i.pricing_category, i.drug_class,
       u.code AS base_unit_code, i.kfa_code, i.non_kfa, i.het_amount, i.is_active
FROM inventory.item i
JOIN inventory.unit u ON u.id = i.base_unit_id
WHERE i.company_id = sqlc.arg('company_id')::uuid AND i.deleted_at IS NULL
  AND (sqlc.narg('item_kind')::text IS NULL OR i.item_kind = sqlc.narg('item_kind')::text)
  AND (sqlc.narg('is_active')::boolean IS NULL OR i.is_active = sqlc.narg('is_active')::boolean)
  AND (sqlc.narg('q')::text IS NULL OR i.code ILIKE '%' || sqlc.narg('q')::text || '%' OR i.name ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY i.name
LIMIT sqlc.arg('row_limit')::integer OFFSET sqlc.arg('row_offset')::integer;

-- name: UpdateInventoryItem :one
UPDATE inventory.item i SET
    name = COALESCE(sqlc.narg('name')::text, i.name),
    kfa_code = COALESCE(sqlc.narg('kfa_code')::text, i.kfa_code),
    kfa_pov_code = COALESCE(sqlc.narg('kfa_pov_code')::text, i.kfa_pov_code),
    non_kfa = COALESCE(sqlc.narg('non_kfa')::boolean, i.non_kfa),
    non_kfa_reason = COALESCE(sqlc.narg('non_kfa_reason')::text, i.non_kfa_reason),
    nie = COALESCE(sqlc.narg('nie')::text, i.nie),
    manufacturer = COALESCE(sqlc.narg('manufacturer')::text, i.manufacturer),
    dosage_form = COALESCE(sqlc.narg('dosage_form')::text, i.dosage_form),
    drug_class = COALESCE(sqlc.narg('drug_class')::text, i.drug_class),
    is_oot = COALESCE(sqlc.narg('is_oot')::boolean, i.is_oot),
    is_fornas = COALESCE(sqlc.narg('is_fornas')::boolean, i.is_fornas),
    fornas_period = COALESCE(sqlc.narg('fornas_period')::text, i.fornas_period),
    is_high_alert = COALESCE(sqlc.narg('is_high_alert')::boolean, i.is_high_alert),
    is_lasa = COALESCE(sqlc.narg('is_lasa')::boolean, i.is_lasa),
    lasa_note = COALESCE(sqlc.narg('lasa_note')::text, i.lasa_note),
    is_generic = COALESCE(sqlc.narg('is_generic')::boolean, i.is_generic),
    het_amount = COALESCE(sqlc.narg('het_amount')::numeric, i.het_amount),
    het_effective_from = COALESCE(sqlc.narg('het_effective_from')::date, i.het_effective_from),
    min_fraction = COALESCE(sqlc.narg('min_fraction')::numeric, i.min_fraction),
    no_crush = COALESCE(sqlc.narg('no_crush')::boolean, i.no_crush),
    bud_after_open_hours = COALESCE(sqlc.narg('bud_after_open_hours')::integer, i.bud_after_open_hours),
    bud_after_reconstitution_hours = COALESCE(sqlc.narg('bud_after_reconstitution_hours')::integer, i.bud_after_reconstitution_hours),
    is_single_dose = COALESCE(sqlc.narg('is_single_dose')::boolean, i.is_single_dose),
    storage_temp = COALESCE(sqlc.narg('storage_temp')::text, i.storage_temp),
    track_batch = COALESCE(sqlc.narg('track_batch')::boolean, i.track_batch),
    track_expiry = COALESCE(sqlc.narg('track_expiry')::boolean, i.track_expiry),
    is_active = COALESCE(sqlc.narg('is_active')::boolean, i.is_active),
    updated_by = sqlc.arg('updated_by')::uuid
WHERE i.id = sqlc.arg('item_id')::uuid
  AND i.row_version = sqlc.arg('expected_row_version')::integer
  AND i.deleted_at IS NULL
RETURNING i.*;

-- name: SoftDeleteInventoryItem :execrows
UPDATE inventory.item i SET deleted_at = now(), deleted_by = sqlc.arg('deleted_by')::uuid
WHERE i.id = sqlc.arg('item_id')::uuid AND i.deleted_at IS NULL;

-- name: ListInventoryItemIngredients :many
SELECT gi.* FROM inventory.item_ingredient gi
WHERE gi.item_id = $1
ORDER BY gi.sort_order, gi.id;

-- name: DeleteInventoryItemIngredients :execrows
DELETE FROM inventory.item_ingredient gi WHERE gi.item_id = $1;

-- name: InsertInventoryItemIngredient :one
INSERT INTO inventory.item_ingredient (
    company_id, item_id, ingredient_code, ingredient_name, strength,
    strength_unit, per_quantity, per_unit, sort_order
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: ListInventoryItemUnits :many
SELECT iu.*, u.code AS unit_code, u.name AS unit_name
FROM inventory.item_unit iu
JOIN inventory.unit u ON u.id = iu.unit_id
WHERE iu.item_id = $1 AND iu.is_active
ORDER BY iu.factor, iu.id;

-- name: GetInventoryItemUnitByID :one
SELECT iu.*, u.code AS unit_code, u.name AS unit_name
FROM inventory.item_unit iu
JOIN inventory.unit u ON u.id = iu.unit_id
WHERE iu.id = $1;

-- name: InsertInventoryItemUnit :one
INSERT INTO inventory.item_unit (
    company_id, item_id, unit_id, factor, kfa_packaging_code,
    is_default_purchase, is_default_issue, created_by
) VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ClearInventoryItemDefaultPurchase :execrows
UPDATE inventory.item_unit iu SET is_default_purchase = false
WHERE iu.item_id = $1 AND iu.is_default_purchase;

-- name: ClearInventoryItemDefaultIssue :execrows
UPDATE inventory.item_unit iu SET is_default_issue = false
WHERE iu.item_id = $1 AND iu.is_default_issue;

-- name: UpdateInventoryItemUnit :one
UPDATE inventory.item_unit iu SET
    is_active = COALESCE(sqlc.narg('is_active')::boolean, iu.is_active),
    is_default_purchase = COALESCE(sqlc.narg('is_default_purchase')::boolean, iu.is_default_purchase),
    is_default_issue = COALESCE(sqlc.narg('is_default_issue')::boolean, iu.is_default_issue),
    kfa_packaging_code = COALESCE(sqlc.narg('kfa_packaging_code')::text, iu.kfa_packaging_code)
WHERE iu.id = sqlc.arg('item_unit_id')::uuid
RETURNING iu.*;
