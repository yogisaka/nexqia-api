-- internal/db/queries/tariff.sql
-- core.service_item, core.rate_component, core.service_rate (docs/07-core-ddl.md §7 "Tarif engine")

-- name: CreateServiceItem :one
INSERT INTO core.service_item (company_id, merchant_id, code, name, item_type, created_by, updated_by, procedure_concept_id)
VALUES ($1, $2, $3, $4, $5, $6, $6, $7)
RETURNING *;

-- name: GetServiceItemByID :one
SELECT * FROM core.service_item WHERE id = $1 AND deleted_at IS NULL;

-- name: ListServiceItemsByMerchant :many
SELECT * FROM core.service_item
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: ListServiceItemsByType :many
SELECT * FROM core.service_item
WHERE merchant_id = $1 AND item_type = $2 AND deleted_at IS NULL
ORDER BY name;

-- name: UpdateServiceItem :one
UPDATE core.service_item
SET name = $2, item_type = $3, procedure_concept_id = $4, is_active = $5, updated_by = $6
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteServiceItem :exec
UPDATE core.service_item
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateRateComponent :one
INSERT INTO core.rate_component (company_id, merchant_id, code, name, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $5)
RETURNING *;

-- name: GetRateComponentByID :one
SELECT * FROM core.rate_component WHERE id = $1 AND deleted_at IS NULL;

-- name: ListRateComponentsByMerchant :many
SELECT * FROM core.rate_component WHERE merchant_id = $1 AND deleted_at IS NULL ORDER BY name;

-- name: UpdateRateComponent :one
UPDATE core.rate_component
SET name = $2, updated_by = $3
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteRateComponent :exec
UPDATE core.rate_component
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListServiceRatesByServiceItem :many
SELECT * FROM core.service_rate
WHERE merchant_id = $1 AND service_item_id = $2 AND deleted_at IS NULL
ORDER BY price_list_id, effective_from DESC;

-- name: CreatePriceList :one
INSERT INTO core.price_list (company_id, merchant_id, code, name, base_price_list_id, adjustment_percent, rounding_unit, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
RETURNING *;

-- name: GetPriceListByID :one
SELECT * FROM core.price_list WHERE id = $1 AND deleted_at IS NULL;

-- name: ListPriceListsByMerchant :many
SELECT * FROM core.price_list WHERE merchant_id = $1 AND deleted_at IS NULL ORDER BY base_price_list_id NULLS FIRST, name;

-- name: UpdatePriceList :one
UPDATE core.price_list
SET name = $2, base_price_list_id = $3, adjustment_percent = $4, rounding_unit = $5, is_active = $6, updated_by = $7
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePriceList :exec
UPDATE core.price_list SET deleted_at = now(), deleted_by = $2 WHERE id = $1 AND deleted_at IS NULL;

-- name: CountDerivedPriceLists :one
SELECT count(*) FROM core.price_list WHERE base_price_list_id = $1 AND deleted_at IS NULL;

-- name: CountLivePricesInList :one
SELECT count(*) FROM core.service_rate
WHERE price_list_id = $1 AND deleted_at IS NULL AND (effective_to IS NULL OR effective_to >= sqlc.arg('on_date')::date);

-- name: ListPriceListAdjustments :many
SELECT item_type, adjustment_percent FROM core.price_list_adjustment WHERE price_list_id = $1 ORDER BY item_type;

-- name: DeletePriceListAdjustments :exec
DELETE FROM core.price_list_adjustment WHERE price_list_id = $1;

-- name: AddPriceListAdjustment :exec
INSERT INTO core.price_list_adjustment (company_id, merchant_id, price_list_id, item_type, adjustment_percent, created_by)
VALUES ($1, $2, $3, $4, $5, $6);

-- name: ListEffectiveRatesForItem :many
SELECT r.rate_component_id, rc.code, rc.name, r.amount, r.effective_from
FROM core.service_rate r
JOIN core.rate_component rc ON rc.id = r.rate_component_id
WHERE r.price_list_id = $1 AND r.service_item_id = $2 AND r.deleted_at IS NULL
  AND r.effective_from <= sqlc.arg('on_date')::date
  AND (r.effective_to IS NULL OR r.effective_to >= sqlc.arg('on_date')::date)
ORDER BY rc.code;

-- name: ListEffectiveRatesForList :many
SELECT r.service_item_id, r.rate_component_id, rc.code, rc.name, r.amount
FROM core.service_rate r
JOIN core.rate_component rc ON rc.id = r.rate_component_id
WHERE r.price_list_id = $1 AND r.deleted_at IS NULL
  AND r.effective_from <= sqlc.arg('on_date')::date
  AND (r.effective_to IS NULL OR r.effective_to >= sqlc.arg('on_date')::date)
ORDER BY r.service_item_id, rc.code;

-- name: LatestPriceVersion :one
-- Newest effective_from of an item in a list (NULL when never priced).
SELECT max(effective_from)::date AS latest FROM core.service_rate
WHERE price_list_id = $1 AND service_item_id = $2 AND deleted_at IS NULL;

-- name: ClosePriceVersion :exec
UPDATE core.service_rate SET effective_to = sqlc.arg('close_on')::date, updated_by = sqlc.arg('updated_by')
WHERE price_list_id = sqlc.arg('price_list_id') AND service_item_id = sqlc.arg('service_item_id')
  AND deleted_at IS NULL AND effective_to IS NULL;

-- name: InsertPriceRow :exec
INSERT INTO core.service_rate (company_id, merchant_id, service_item_id, rate_component_id, price_list_id, amount, effective_from, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8);

-- name: DeletePriceVersion :execrows
UPDATE core.service_rate SET deleted_at = now(), deleted_by = sqlc.arg('deleted_by')
WHERE price_list_id = sqlc.arg('price_list_id') AND service_item_id = sqlc.arg('service_item_id')
  AND effective_from = sqlc.arg('effective_from')::date AND deleted_at IS NULL;

-- name: ReopenPreviousPriceVersion :exec
-- After deleting a future version: the newest remaining version is open again.
-- Alias r + typed args: sqlc rejects the unqualified form ("price_list_id is ambiguous", run 2026-10-07).
UPDATE core.service_rate r SET effective_to = NULL, updated_by = sqlc.arg('updated_by')
WHERE r.price_list_id = sqlc.arg('price_list_id')::uuid AND r.service_item_id = sqlc.arg('service_item_id')::uuid
  AND r.deleted_at IS NULL
  AND r.effective_from = (SELECT max(r2.effective_from) FROM core.service_rate r2
                          WHERE r2.price_list_id = sqlc.arg('price_list_id')::uuid
                            AND r2.service_item_id = sqlc.arg('service_item_id')::uuid
                            AND r2.deleted_at IS NULL);

-- name: ListServiceItemsForGrid :many
SELECT * FROM core.service_item
WHERE merchant_id = $1 AND deleted_at IS NULL AND is_active
  AND (sqlc.narg('item_type')::text IS NULL OR item_type = sqlc.narg('item_type')::text)
  AND (sqlc.narg('q')::text IS NULL OR code ILIKE '%' || sqlc.narg('q')::text || '%' OR name ILIKE '%' || sqlc.narg('q')::text || '%')
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: CreateRateComponentIfMissing :execrows
INSERT INTO core.rate_component (company_id, merchant_id, code, name, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $5)
ON CONFLICT (merchant_id, code) DO NOTHING;

-- Platform price templates (spec 2026-10-07-tariff-price-lists §6). RLS
-- filters template scope; platform rows are visible to every tenant.

-- name: ListPriceTemplates :many
SELECT * FROM core.template WHERE kind = 'price_list' AND is_active ORDER BY name;

-- name: ListTemplatePriceItems :many
SELECT * FROM core.template_price_item WHERE template_id = $1 ORDER BY sort_order;

-- name: GetServiceItemByMerchantCode :one
SELECT * FROM core.service_item WHERE merchant_id = $1 AND code = $2 AND deleted_at IS NULL;

-- name: InsertTemplateApplication :exec
INSERT INTO core.template_application
    (company_id, template_id, template_version, applied_by, roles_created, roles_skipped)
VALUES ($1, $2, $3, $4, $5, $6);
