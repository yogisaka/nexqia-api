-- internal/db/queries/billing.sql
-- Payer CRUD + service rules (000010), payer plans / rules / limits and BPJS
-- item coverage + merchant billing profile (000068) — spec
-- 2026-10-07-tariff-b1-billing-core §3, §6.

-- name: GetPayerByID :one
SELECT * FROM core.payer WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePayer :one
INSERT INTO core.payer (company_id, merchant_id, code, name, payer_type, price_list_id, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING *;

-- name: UpdatePayer :one
UPDATE core.payer
SET name = $2, is_active = $3, price_list_id = $4, updated_by = $5
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePayer :exec
UPDATE core.payer SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountPayerCodeConflicts :one
-- exclude_id is NULL on create.
SELECT count(*) FROM core.payer
WHERE merchant_id = $1 AND code = $2 AND deleted_at IS NULL
  AND id <> COALESCE(sqlc.narg('exclude_id')::uuid, '00000000-0000-0000-0000-000000000000'::uuid);

-- name: CountBpjsPayers :one
-- Live BPJS payers of a merchant, optionally excluding one payer (update).
SELECT count(*) FROM core.payer
WHERE merchant_id = $1 AND payer_type = 'bpjs' AND deleted_at IS NULL
  AND id <> COALESCE(sqlc.narg('exclude_id')::uuid, '00000000-0000-0000-0000-000000000000'::uuid);

-- name: CountPayerActiveVisits :one
-- Guarantor rows on visits that are not yet discharged/cancelled.
SELECT count(*) FROM operations.admission_guarantor g
JOIN operations.admission a ON a.id = g.admission_id
WHERE g.payer_id = $1 AND a.deleted_at IS NULL
  AND a.status NOT IN ('discharged', 'cancelled');

-- Payer service rules (blacklist / contract discount, full-set replace).

-- name: ListPayerServiceRules :many
SELECT * FROM core.payer_service_rule
WHERE payer_id = $1 AND is_active
ORDER BY service_item_id;

-- name: DeletePayerServiceRules :exec
DELETE FROM core.payer_service_rule WHERE payer_id = $1;

-- name: InsertPayerServiceRule :exec
INSERT INTO core.payer_service_rule
    (company_id, merchant_id, payer_id, service_item_id, rule_type, discount_percent, discount_amount, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- Payer plans (000068).

-- name: ListPayerPlans :many
SELECT * FROM core.payer_plan
WHERE payer_id = $1 AND deleted_at IS NULL
ORDER BY code;

-- name: GetPayerPlanByID :one
SELECT * FROM core.payer_plan WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePayerPlan :one
INSERT INTO core.payer_plan
    (company_id, merchant_id, payer_id, code, name, requires_gl, annual_limit, plan_year_start_month, is_active, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10)
RETURNING *;

-- name: UpdatePayerPlan :one
UPDATE core.payer_plan
SET code = $2, name = $3, requires_gl = $4, annual_limit = $5,
    plan_year_start_month = $6, is_active = $7, updated_by = $8
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePayerPlan :exec
UPDATE core.payer_plan SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CountPlanCoverages :one
-- Active patient memberships pointing at the plan.
SELECT count(*) FROM core.patient_coverage
WHERE plan_id = $1 AND deleted_at IS NULL;

-- name: ListPlanRules :many
SELECT * FROM core.payer_plan_rule WHERE plan_id = $1 ORDER BY care_type;

-- name: DeletePlanRules :exec
DELETE FROM core.payer_plan_rule WHERE plan_id = $1;

-- name: InsertPlanRule :exec
INSERT INTO core.payer_plan_rule
    (company_id, merchant_id, plan_id, care_type, copay_percent, copay_max_per_claim, deductible_per_claim, visit_limit)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListPlanLimits :many
SELECT * FROM core.payer_plan_limit WHERE plan_id = $1 ORDER BY item_type;

-- name: DeletePlanLimits :exec
DELETE FROM core.payer_plan_limit WHERE plan_id = $1;

-- name: InsertPlanLimit :exec
INSERT INTO core.payer_plan_limit
    (company_id, merchant_id, plan_id, item_type, limit_per_visit)
VALUES ($1, $2, $3, $4, $5);

-- BPJS item coverage (full-set replace + template import).

-- name: ListPayerItemCoverage :many
SELECT * FROM core.payer_item_coverage WHERE payer_id = $1
ORDER BY item_type NULLS LAST, service_item_id;

-- name: DeletePayerItemCoverage :exec
DELETE FROM core.payer_item_coverage WHERE payer_id = $1;

-- name: InsertPayerItemCoverage :exec
INSERT INTO core.payer_item_coverage
    (company_id, merchant_id, payer_id, service_item_id, item_type, coverage, claim_type)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: ListMerchantItemsInBPJSTemplate :many
-- Merchant service items whose code exists in the bpjs_nonkapitasi_2023
-- price template (Permenkes 3/2023, migration 000063).
SELECT si.id, si.code
FROM core.service_item si
JOIN core.template_price_item tpi ON tpi.code = si.code
JOIN core.template t ON t.id = tpi.template_id
WHERE si.merchant_id = $1 AND si.deleted_at IS NULL
  AND t.code = 'bpjs_nonkapitasi_2023' AND t.kind = 'price_list';

-- Merchant billing profile (facility BPJS profile, 000068 §3.1).

-- name: UpdateMerchantBillingProfile :one
UPDATE core.merchant
SET facility_type = $2, bpjs_cooperation = $3, bpjs_network_parent = $4, default_price_list_id = $5
WHERE id = $1
RETURNING *;
