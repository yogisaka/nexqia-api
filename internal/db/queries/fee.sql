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

-- SetChargeSplitComponents stores the per-component portions ([{"code",
-- "amount"}] jsonb) of one freshly inserted charge_split row (spec
-- 2026-10-07-tariff-d §4.2: fee gross keys on the MEDIS component).
-- name: SetChargeSplitComponents :exec
UPDATE operations.charge_split
SET components = $3
WHERE charge_id = $1 AND seq = $2;

-- Charge performers (spec 2026-10-07-tariff-d §3, per-charge pelaksana).

-- name: InsertChargePerformer :one
INSERT INTO operations.charge_performer (company_id, merchant_id, charge_id, physician_id, role, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: DeleteChargePerformers :exec
DELETE FROM operations.charge_performer WHERE charge_id = $1;

-- name: ListChargePerformersForAdmission :many
SELECT cp.charge_id, cp.physician_id, cp.role, pe.full_name AS physician_name
FROM operations.charge_performer cp
JOIN operations.charge ch ON ch.id = cp.charge_id
JOIN core.physician ph ON ph.id = cp.physician_id
JOIN core.person pe ON pe.id = ph.person_id
WHERE ch.admission_id = $1
ORDER BY cp.charge_id, cp.created_at, cp.id;

-- name: ListChargePerformersForAccount :many
SELECT cp.charge_id, cp.physician_id, cp.role
FROM operations.charge_performer cp
JOIN operations.charge ch ON ch.id = cp.charge_id
WHERE ch.account_id = $1
ORDER BY cp.charge_id, cp.created_at, cp.id;

-- Fee computation inputs (spec 2026-10-07-tariff-d §4.1–4.2).

-- name: ListChargeFeeLinesForAccount :many
SELECT ch.id, ch.service_item_id, si.item_type, ch.quantity, ch.service_date
FROM operations.charge ch
JOIN core.service_item si ON si.id = ch.service_item_id
WHERE ch.account_id = $1 AND ch.status = 'active'
ORDER BY ch.service_date, ch.created_at, ch.id;

-- name: ListSplitsWithComponentsForCharge :many
SELECT party, payer_id, coverage, amount, components
FROM operations.charge_split
WHERE charge_id = $1
ORDER BY seq;

-- name: CountFeesInProcessByAccount :one
SELECT count(*) FROM operations.practitioner_fee
WHERE account_id = $1 AND status <> 'open';

-- name: DeleteFeesByAccount :exec
DELETE FROM operations.practitioner_fee WHERE account_id = $1;

-- name: InsertPractitionerFee :one
INSERT INTO operations.practitioner_fee (company_id, merchant_id, account_id, charge_id, physician_id, role, service_date, gross_amount, fee_amount, rule_id, rule_snapshot)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;
