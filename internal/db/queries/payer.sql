-- internal/db/queries/payer.sql
-- core.supplier, core.manufacturer, core.diet_type, core.payer, core.payer_service_rule
-- (docs/07-core-ddl.md §8 "Payer / Penjamin")

-- name: CreateSupplier :one
INSERT INTO core.supplier (
    company_id, merchant_id, code, name, pic_name, pic_phone, address, npwp, bank_account,
    credit_limit, lead_time_days, lead_time_cito_days, payment_term_days, contract_start_date,
    contract_end_date, created_by, updated_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $16
)
RETURNING *;

-- name: GetSupplierByID :one
SELECT * FROM core.supplier WHERE id = $1 AND deleted_at IS NULL;

-- name: ListSuppliersByMerchant :many
SELECT * FROM core.supplier
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: UpdateSupplier :one
UPDATE core.supplier
SET name = $2, pic_name = $3, pic_phone = $4, address = $5, npwp = $6, bank_account = $7,
    credit_limit = $8, lead_time_days = $9, lead_time_cito_days = $10, payment_term_days = $11,
    contract_start_date = $12, contract_end_date = $13, is_active = $14, updated_by = $15
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteSupplier :exec
UPDATE core.supplier
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateManufacturer :one
INSERT INTO core.manufacturer (company_id, merchant_id, code, name, abbreviation, address, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING *;

-- name: GetManufacturerByID :one
SELECT * FROM core.manufacturer WHERE id = $1;

-- name: ListManufacturersByMerchant :many
SELECT * FROM core.manufacturer WHERE merchant_id = $1 AND is_active ORDER BY name;

-- name: UpdateManufacturer :one
UPDATE core.manufacturer
SET name = $2, abbreviation = $3, address = $4, updated_by = $5
WHERE id = $1
RETURNING *;

-- name: DeactivateManufacturer :exec
UPDATE core.manufacturer SET is_active = false, updated_by = $2 WHERE id = $1;

-- name: CreateDietType :one
INSERT INTO core.diet_type (company_id, merchant_id, code, name, default_calorie, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $6)
RETURNING *;

-- name: GetDietTypeByID :one
SELECT * FROM core.diet_type WHERE id = $1;

-- name: ListDietTypesByMerchant :many
SELECT * FROM core.diet_type WHERE merchant_id = $1 AND is_active ORDER BY name;

-- name: UpdateDietType :one
UPDATE core.diet_type
SET name = $2, default_calorie = $3, updated_by = $4
WHERE id = $1
RETURNING *;

-- name: DeactivateDietType :exec
UPDATE core.diet_type SET is_active = false, updated_by = $2 WHERE id = $1;

-- name: CreatePayer :one
INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $6)
RETURNING *;

-- name: GetPayerByID :one
SELECT * FROM core.payer WHERE id = $1 AND deleted_at IS NULL;

-- name: ListPayersByMerchant :many
SELECT * FROM core.payer WHERE merchant_id = $1 AND deleted_at IS NULL ORDER BY name;

-- name: ListPayersByType :many
SELECT * FROM core.payer WHERE merchant_id = $1 AND payer_type = $2 AND deleted_at IS NULL ORDER BY name;

-- name: UpdatePayer :one
UPDATE core.payer
SET name = $2, payer_type = $3, is_active = $4, updated_by = $5
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePayer :exec
UPDATE core.payer
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePayerServiceRule :one
INSERT INTO core.payer_service_rule (company_id, merchant_id, payer_id, service_item_id, rule_type, discount_percent, discount_amount, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $8)
RETURNING *;

-- name: GetPayerServiceRuleByID :one
SELECT * FROM core.payer_service_rule WHERE id = $1;

-- name: ListPayerServiceRulesByPayer :many
SELECT * FROM core.payer_service_rule WHERE merchant_id = $1 AND payer_id = $2 AND is_active;

-- name: ListActiveRulesForServiceItem :many
SELECT * FROM core.payer_service_rule
WHERE merchant_id = $1 AND service_item_id = $2 AND payer_id = $3 AND is_active;

-- name: UpdatePayerServiceRule :one
UPDATE core.payer_service_rule
SET discount_percent = $2, discount_amount = $3, updated_by = $4
WHERE id = $1
RETURNING *;

-- name: DeactivatePayerServiceRule :exec
UPDATE core.payer_service_rule SET is_active = false, updated_by = $2 WHERE id = $1;
