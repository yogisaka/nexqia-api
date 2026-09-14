-- internal/db/queries/tariff.sql
-- core.service_item, core.rate_component, core.service_rate (docs/07-core-ddl.md §7 "Tarif engine")

-- name: CreateServiceItem :one
INSERT INTO core.service_item (company_id, merchant_id, code, name, item_type, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $6)
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
SET name = $2, item_type = $3, is_active = $4, updated_by = $5
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

-- name: CreateServiceRate :one
INSERT INTO core.service_rate (company_id, merchant_id, service_item_id, rate_component_id, payer_class, amount, effective_from, effective_to, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
RETURNING *;

-- name: GetServiceRateByID :one
SELECT * FROM core.service_rate WHERE id = $1 AND deleted_at IS NULL;

-- name: ListServiceRatesByServiceItem :many
SELECT * FROM core.service_rate
WHERE merchant_id = $1 AND service_item_id = $2 AND deleted_at IS NULL
ORDER BY payer_class, effective_from DESC;

-- name: GetEffectiveServiceRate :one
SELECT * FROM core.service_rate
WHERE merchant_id = $1 AND service_item_id = $2 AND payer_class = $3
    AND deleted_at IS NULL
    AND effective_from <= $4
    AND (effective_to IS NULL OR effective_to >= $4)
ORDER BY effective_from DESC
LIMIT 1;

-- name: UpdateServiceRate :one
UPDATE core.service_rate
SET amount = $2, effective_to = $3, updated_by = $4
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteServiceRate :exec
UPDATE core.service_rate
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
