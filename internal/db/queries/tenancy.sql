-- internal/db/queries/tenancy.sql
-- core.company, core.merchant (docs/07-core-ddl.md §1 "Tenancy root")

-- name: CreateCompany :one
INSERT INTO core.company (code, name, created_by, updated_by)
VALUES ($1, $2, $3, $3)
RETURNING *;

-- name: GetCompanyByID :one
SELECT * FROM core.company WHERE id = $1 AND deleted_at IS NULL;

-- name: GetCompanyByCode :one
SELECT * FROM core.company WHERE code = $1 AND deleted_at IS NULL;

-- name: ListCompanies :many
SELECT * FROM core.company
WHERE deleted_at IS NULL
ORDER BY name
LIMIT $1 OFFSET $2;

-- name: UpdateCompany :one
UPDATE core.company
SET name = $2, is_active = $3, updated_by = $4
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteCompany :exec
UPDATE core.company
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateMerchant :one
INSERT INTO core.merchant (company_id, code, name, kemkes_facility_code, bpjs_ppk_code, timezone, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING *;

-- name: GetMerchantByID :one
SELECT * FROM core.merchant WHERE id = $1 AND deleted_at IS NULL;

-- name: ListMerchantsByCompany :many
SELECT * FROM core.merchant
WHERE company_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: UpdateMerchant :one
UPDATE core.merchant
SET name = $2, kemkes_facility_code = $3, bpjs_ppk_code = $4, timezone = $5, is_active = $6, updated_by = $7
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteMerchant :exec
UPDATE core.merchant
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
