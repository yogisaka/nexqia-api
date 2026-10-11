-- internal/db/queries/fee.sql
-- Practitioner fee rules (core.practitioner_fee_rule, migration 000070) and
-- the physician employment extensions (profession, employment_type, PTKP,
-- NPWP) of spec 2026-10-07-tariff-d-practitioner-fees-tax §3.

-- name: SetPhysicianEmployment :one
UPDATE core.physician
SET profession = COALESCE(NULLIF(sqlc.narg('profession')::text, ''), profession),
    employment_type = COALESCE(NULLIF(sqlc.narg('employment_type')::text, ''), employment_type),
    ptkp_status = COALESCE(NULLIF(sqlc.narg('ptkp_status')::text, ''), ptkp_status),
    npwp = COALESCE(NULLIF(sqlc.narg('npwp')::text, ''), npwp)
WHERE id = sqlc.arg('id') AND deleted_at IS NULL
RETURNING *;

-- name: ListPhysiciansByProfession :many
-- Same shape as ListPhysiciansByMerchant, filtered by profession.
SELECT p.*,
       pe.full_name AS person_full_name,
       pe.photo_url AS person_photo_url,
       c.display    AS specialty_display
FROM core.physician p
JOIN core.person pe ON pe.id = p.person_id
LEFT JOIN terminology.concept c ON c.id = p.specialty_concept_id
WHERE p.merchant_id = $1 AND p.deleted_at IS NULL AND p.profession = $2
ORDER BY pe.full_name
LIMIT $3 OFFSET $4;

-- name: CreateFeeRule :one
INSERT INTO core.practitioner_fee_rule (company_id, merchant_id, physician_id, service_item_id, item_type, payer_id, performer_role, basis, component_code, percent, amount, effective_from, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING *;

-- name: ListFeeRulesByMerchant :many
SELECT * FROM core.practitioner_fee_rule
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY effective_from DESC, id;

-- name: GetFeeRuleByID :one
SELECT * FROM core.practitioner_fee_rule WHERE id = $1 AND deleted_at IS NULL;

-- name: GetFeeRulePayerMerchant :one
SELECT merchant_id FROM core.payer WHERE id = $1 AND deleted_at IS NULL;

-- name: UpdateFeeRule :one
UPDATE core.practitioner_fee_rule
SET physician_id = $2, service_item_id = $3, item_type = $4, payer_id = $5,
    performer_role = $6, basis = $7, component_code = $8, percent = $9,
    amount = $10, effective_from = $11, updated_by = $12
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteFeeRule :exec
UPDATE core.practitioner_fee_rule
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
