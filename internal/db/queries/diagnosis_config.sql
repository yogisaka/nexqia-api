-- internal/db/queries/diagnosis_config.sql
-- Configurable diagnosis coding standard (migrations/000033). One
-- company-level default row (merchant_id IS NULL) plus an optional
-- per-merchant override — same shape as mrn_config.sql.

-- name: GetDiagnosisCodeSystem :one
-- Merchant override first (if $1 matches a row), else the company default.
-- Returns no rows if neither exists — caller falls back to a hardcoded default.
(SELECT code_system FROM core.diagnosis_config WHERE merchant_id = $1)
UNION ALL
(SELECT code_system FROM core.diagnosis_config WHERE company_id = $2 AND merchant_id IS NULL)
LIMIT 1;

-- name: GetCompanyDiagnosisConfig :one
SELECT * FROM core.diagnosis_config WHERE company_id = $1 AND merchant_id IS NULL;

-- name: GetMerchantDiagnosisConfig :one
SELECT * FROM core.diagnosis_config WHERE merchant_id = $1;

-- name: UpsertCompanyDiagnosisConfig :one
INSERT INTO core.diagnosis_config (company_id, merchant_id, code_system, created_by, updated_by)
VALUES ($1, NULL, $2, $3, $3)
ON CONFLICT (company_id) WHERE merchant_id IS NULL
DO UPDATE SET code_system = EXCLUDED.code_system, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: UpsertMerchantDiagnosisConfig :one
INSERT INTO core.diagnosis_config (company_id, merchant_id, code_system, created_by, updated_by)
VALUES ($1, $2, $3, $4, $4)
ON CONFLICT (merchant_id) WHERE merchant_id IS NOT NULL
DO UPDATE SET code_system = EXCLUDED.code_system, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: DeleteMerchantDiagnosisConfig :exec
DELETE FROM core.diagnosis_config WHERE merchant_id = $1;

-- name: ListCodeSystems :many
-- Discovery/validation list for diagnosis-config admin (and anything else
-- that needs to know which terminology.code_system values are valid).
SELECT * FROM terminology.code_system WHERE is_active ORDER BY name;
