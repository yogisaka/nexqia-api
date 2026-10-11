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

-- Patient coverage (kepesertaan, 000068 §3.7).

-- name: ListPatientCoveragesByPerson :many
SELECT pc.*, p.code AS payer_code, p.name AS payer_name, p.payer_type
FROM core.patient_coverage pc
JOIN core.payer p ON p.id = pc.payer_id
WHERE pc.person_id = $1 AND pc.deleted_at IS NULL
ORDER BY pc.created_at, pc.id;

-- name: GetPatientCoverageByID :one
SELECT * FROM core.patient_coverage WHERE id = $1 AND deleted_at IS NULL;

-- name: GetPatientCoverageDetail :one
SELECT pc.*, p.code AS payer_code, p.name AS payer_name, p.payer_type
FROM core.patient_coverage pc
JOIN core.payer p ON p.id = pc.payer_id
WHERE pc.id = $1 AND pc.deleted_at IS NULL;

-- name: CreatePatientCoverage :one
INSERT INTO core.patient_coverage
    (company_id, merchant_id, person_id, payer_id, plan_id, member_number, bpjs_class,
     valid_from, valid_to, tpa_remaining_annual, tpa_remaining_as_of, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
RETURNING *;

-- name: UpdatePatientCoverage :one
UPDATE core.patient_coverage
SET payer_id = $2, plan_id = $3, member_number = $4, bpjs_class = $5,
    valid_from = $6, valid_to = $7, tpa_remaining_annual = $8, tpa_remaining_as_of = $9,
    is_active = $10, updated_by = $11
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- Visit guarantors (full-set replace, spec §6).

-- name: ListAdmissionGuarantors :many
SELECT * FROM operations.admission_guarantor WHERE admission_id = $1 ORDER BY sequence;

-- name: DeleteAdmissionGuarantors :exec
DELETE FROM operations.admission_guarantor WHERE admission_id = $1;

-- name: InsertAdmissionGuarantor :exec
INSERT INTO operations.admission_guarantor
    (admission_id, payer_id, coverage_id, plan_id, policy_number, guarantor_name, sequence, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: SetAdmissionPrimaryPayer :exec
UPDATE operations.admission SET primary_payer_id = $2, updated_by = $3 WHERE id = $1;

-- Billing account (admission-scoped, 000068 §3.10).

-- name: GetBillingAccountByAdmission :one
SELECT * FROM operations.billing_account WHERE admission_id = $1;

-- Guarantee letters (full-set soft replace, spec §3.9/§6).

-- name: ListGuaranteeLetters :many
SELECT * FROM operations.guarantee_letter
WHERE admission_id = $1 AND deleted_at IS NULL
ORDER BY created_at, id;

-- name: SoftDeleteGuaranteeLetters :exec
UPDATE operations.guarantee_letter SET deleted_at = now(), deleted_by = $2
WHERE admission_id = $1 AND deleted_at IS NULL;

-- Billing account API (spec §5): account rows, charges, splits, consents and
-- the annual-usage sum feeding the split engine.

-- name: InsertBillingAccountIfAbsent :exec
-- Two concurrent first charges on a new visit: the loser of the race inserts
-- nothing and reads the winner's row FOR UPDATE below.
INSERT INTO operations.billing_account (company_id, merchant_id, admission_id)
VALUES ($1, $2, $3)
ON CONFLICT (admission_id) DO NOTHING;

-- name: GetBillingAccountForUpdate :one
SELECT * FROM operations.billing_account WHERE id = $1 FOR UPDATE;

-- name: ListActiveChargesByAccount :many
SELECT * FROM operations.charge
WHERE account_id = $1 AND status = 'active'
ORDER BY service_date, created_at, id;

-- name: ListChargesForAdmission :many
SELECT ch.*, si.code AS item_code, si.name AS item_name, si.item_type AS item_type
FROM operations.charge ch
JOIN core.service_item si ON si.id = ch.service_item_id
WHERE ch.admission_id = $1
ORDER BY ch.service_date, ch.created_at, ch.id;

-- name: ListSplitsForAdmission :many
SELECT cs.* FROM operations.charge_split cs
JOIN operations.charge ch ON ch.id = cs.charge_id
WHERE ch.admission_id = $1
ORDER BY cs.charge_id, cs.seq;

-- name: GetChargeByID :one
SELECT * FROM operations.charge WHERE id = $1;

-- name: InsertCharge :one
INSERT INTO operations.charge
    (company_id, merchant_id, account_id, admission_id, service_item_id, quantity, service_date, source, note, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, 'manual', $8, $9)
RETURNING *;

-- name: VoidCharge :one
UPDATE operations.charge
SET status = 'void', void_reason = $2, voided_by = $3, voided_at = now(), updated_by = $3
WHERE id = $1 AND status = 'active'
RETURNING *;

-- name: DeleteChargeSplitsByAccount :exec
DELETE FROM operations.charge_split cs
USING operations.charge ch
WHERE cs.charge_id = ch.id AND ch.account_id = $1;

-- name: InsertChargeSplit :exec
INSERT INTO operations.charge_split
    (company_id, merchant_id, charge_id, seq, party, payer_id, coverage, claim_type,
     price_list_id, unit_amount, gross_amount, discount_amount, amount, shadow_amount, note)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15);

-- name: FinalizeBillingAccount :one
UPDATE operations.billing_account
SET status = 'finalized', finalized_at = now(), finalized_by = $2,
    patient_total = $3, payer_total = $4, updated_by = $2
WHERE id = $1 AND status = 'open'
RETURNING *;

-- name: ReopenBillingAccount :one
-- Totals and finalized_* MUST be nulled: chk_billing_account_final.
UPDATE operations.billing_account
SET status = 'open', finalized_at = NULL, finalized_by = NULL,
    patient_total = NULL, payer_total = NULL,
    reopened_count = reopened_count + 1, last_reopen_reason = $2, updated_by = $3
WHERE id = $1 AND status = 'finalized'
RETURNING *;

-- name: ListBillingConsentsByAdmission :many
SELECT * FROM operations.billing_consent
WHERE admission_id = $1
ORDER BY recorded_at, id;

-- name: InsertBillingConsent :one
INSERT INTO operations.billing_consent
    (company_id, merchant_id, admission_id, charge_id, kind, consented_by_name, relation, recorded_by)
VALUES ($1, $2, $3, $4, 'bpjs_excluded', $5, $6, $7)
RETURNING *;

-- name: CountActiveChargesByAdmission :one
SELECT count(*) FROM operations.charge
WHERE admission_id = $1 AND status = 'active';

-- name: SumPayerAnnualUsage :one
-- Payer-side amounts already frozen into OTHER finalized accounts of the same
-- person under the SAME plan within the plan's running year (window computed
-- in Go from plan_year_start_month and matched against each charge's
-- service_date — the plan year the SERVICE belongs to, not the year the
-- split was computed/finalized in). The guarantor row of the past visit
-- must resolve to the same plan — directly via plan_id or, when the row was
-- stored without one, through its coverage's plan_id — so usage of another
-- plan of the same payer never eats this plan's annual limit.
SELECT COALESCE(sum(cs.amount), 0)::numeric AS used
FROM operations.charge_split cs
JOIN operations.charge ch ON ch.id = cs.charge_id
JOIN operations.billing_account ba ON ba.id = ch.account_id
JOIN operations.admission a ON a.id = ba.admission_id
WHERE cs.party = 'payer' AND cs.payer_id = $1
  AND a.person_id = $2 AND ba.admission_id <> $3
  AND ba.status = 'finalized' AND ch.status = 'active'
  AND ch.service_date >= $4 AND ch.service_date < $5
  AND EXISTS (
      SELECT 1 FROM operations.admission_guarantor ag
      LEFT JOIN core.patient_coverage coverage ON coverage.id = ag.coverage_id
      WHERE ag.admission_id = a.id AND ag.payer_id = cs.payer_id
        AND COALESCE(ag.plan_id, coverage.plan_id) = $6);

-- name: InsertGuaranteeLetter :exec
INSERT INTO operations.guarantee_letter
    (company_id, merchant_id, admission_id, payer_id, gl_number, amount_limit,
     valid_from, valid_to, note, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $10);

-- Payer receivables (spec 2026-10-07-tariff-b2-receivables-claims §4.1/§4.5).

-- name: SumPayerSplitsForAccount :many
-- One row per payer bearing a positive share of the account's active charges:
-- the receivable amount, the distinct claim types and the service date range.
SELECT cs.payer_id,
       p.name AS payer_name,
       p.payer_type,
       p.payment_term_days,
       COALESCE(sum(cs.amount), 0)::numeric AS total_amount,
       COALESCE(array_agg(DISTINCT cs.claim_type)
                FILTER (WHERE cs.claim_type IS NOT NULL AND cs.claim_type <> ''), '{}')::text[] AS claim_types,
       min(ch.service_date)::date AS service_date_from,
       max(ch.service_date)::date AS service_date_to
FROM operations.charge_split cs
JOIN operations.charge ch ON ch.id = cs.charge_id
JOIN core.payer p ON p.id = cs.payer_id
WHERE ch.account_id = $1 AND ch.status = 'active' AND cs.party = 'payer' AND cs.amount > 0
GROUP BY cs.payer_id, p.name, p.payer_type, p.payment_term_days
ORDER BY p.name;

-- name: InsertReceivable :one
INSERT INTO operations.receivable
    (company_id, merchant_id, account_id, admission_id, payer_id,
     service_date_from, service_date_to, amount, claim_types, due_date, expires_on, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: ListReceivablesByAccount :many
SELECT * FROM operations.receivable WHERE account_id = $1;

-- name: CountReceivablesInProcessByAccount :one
-- Reopen guard (§4.1.2): any receivable that is no longer fresh `open`, or
-- that already carries an allocation/adjustment, blocks reopening.
SELECT count(*) FROM operations.receivable r
WHERE r.account_id = $1
  AND (r.status <> 'open'
       OR EXISTS (SELECT 1 FROM operations.payer_payment_allocation a WHERE a.receivable_id = r.id)
       OR EXISTS (SELECT 1 FROM operations.receivable_adjustment j WHERE j.receivable_id = r.id));

-- name: DeleteReceivablesByAccount :exec
DELETE FROM operations.receivable WHERE account_id = $1;

-- name: GetReceivableByID :one
SELECT r.*, p.name AS payer_name, p.payer_type,
       a.visit_no AS admission_visit_no,
       per.id AS person_id, per.full_name AS person_full_name,
       per.medical_record_no AS person_mrn
FROM operations.receivable r
JOIN core.payer p ON p.id = r.payer_id
JOIN operations.admission a ON a.id = r.admission_id
JOIN core.person per ON per.id = a.person_id
WHERE r.id = $1;

-- name: ListReceivablesByMerchant :many
SELECT r.*, p.name AS payer_name, p.payer_type,
       a.visit_no AS admission_visit_no,
       per.id AS person_id, per.full_name AS person_full_name,
       per.medical_record_no AS person_mrn
FROM operations.receivable r
JOIN core.payer p ON p.id = r.payer_id
JOIN operations.admission a ON a.id = r.admission_id
JOIN core.person per ON per.id = a.person_id
WHERE r.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.narg(payer_id)::uuid IS NULL OR r.payer_id = sqlc.narg(payer_id))
  AND (sqlc.narg(status)::text IS NULL OR r.status = sqlc.narg(status))
  AND (sqlc.narg(date_from)::date IS NULL OR r.service_date_to >= sqlc.narg(date_from))
  AND (sqlc.narg(date_to)::date IS NULL OR r.service_date_to <= sqlc.narg(date_to))
ORDER BY r.due_date, r.id
LIMIT sqlc.arg(row_limit)::integer OFFSET sqlc.arg(row_offset)::integer;

-- name: ReceivableAgingByMerchant :many
-- AR aging per payer (§4.5): outstanding balance (> 0) bucketed by
-- as_of - due_date in days; disputed receivables live in their own column.
WITH bal AS (
    SELECT r.payer_id, p.name AS payer_name,
           (r.status = 'disputed') AS is_disputed,
           (sqlc.arg(as_of)::date - r.due_date) AS overdue_days,
           (r.amount - r.paid_amount - r.adjusted_amount) AS balance
    FROM operations.receivable r
    JOIN core.payer p ON p.id = r.payer_id
    WHERE r.merchant_id = sqlc.arg(merchant_id)
      AND r.amount - r.paid_amount - r.adjusted_amount > 0
)
SELECT payer_id, payer_name,
    COALESCE(sum(balance) FILTER (WHERE NOT is_disputed AND overdue_days <= 30), 0)::numeric AS d0_30,
    COALESCE(sum(balance) FILTER (WHERE NOT is_disputed AND overdue_days BETWEEN 31 AND 60), 0)::numeric AS d31_60,
    COALESCE(sum(balance) FILTER (WHERE NOT is_disputed AND overdue_days BETWEEN 61 AND 90), 0)::numeric AS d61_90,
    COALESCE(sum(balance) FILTER (WHERE NOT is_disputed AND overdue_days > 90), 0)::numeric AS over_90,
    COALESCE(sum(balance) FILTER (WHERE is_disputed), 0)::numeric AS disputed
FROM bal
GROUP BY payer_id, payer_name
ORDER BY payer_name;

-- name: ListExpiringReceivables :many
-- BPJS receivables (spec §4.5/§7.2: expiry = last service date + 6 months)
-- with an outstanding balance, expiring within `days` days of as_of.
SELECT r.*, p.name AS payer_name, p.payer_type,
       a.visit_no AS admission_visit_no,
       per.id AS person_id, per.full_name AS person_full_name,
       per.medical_record_no AS person_mrn
FROM operations.receivable r
JOIN core.payer p ON p.id = r.payer_id
JOIN operations.admission a ON a.id = r.admission_id
JOIN core.person per ON per.id = a.person_id
WHERE r.merchant_id = sqlc.arg(merchant_id)
  AND p.payer_type = 'bpjs'
  AND r.expires_on IS NOT NULL AND r.expires_on <= sqlc.arg(expires_before)
  AND r.amount - r.paid_amount - r.adjusted_amount > 0
ORDER BY r.expires_on, r.id;

-- name: GetReceivableForUpdate :one
SELECT * FROM operations.receivable WHERE id = $1 FOR UPDATE;

-- name: SumReceivableAllocations :one
SELECT COALESCE(sum(amount), 0)::numeric AS paid
FROM operations.payer_payment_allocation WHERE receivable_id = $1;

-- name: SumReceivableAdjustments :one
SELECT COALESCE(sum(amount), 0)::numeric AS adjusted
FROM operations.receivable_adjustment WHERE receivable_id = $1;

-- name: GetClaimBatchByID :one
SELECT * FROM operations.claim_batch WHERE id = $1;

-- name: UpdateReceivableTotalsAndStatus :one
UPDATE operations.receivable
SET paid_amount = $2, adjusted_amount = $3, status = $4, updated_by = $5
WHERE id = $1
RETURNING *;

-- name: SetReceivableDispute :one
-- Status and reason move together (chk_receivable_dispute ties `disputed` to
-- a non-empty reason); refreshReceivableStatus re-derives the final status.
UPDATE operations.receivable
SET status = CASE WHEN btrim(coalesce($2, '')) <> '' THEN 'disputed' ELSE 'open' END,
    dispute_reason = $2, updated_by = $3
WHERE id = $1
RETURNING *;

-- name: UpdateReceivableDocuments :one
UPDATE operations.receivable
SET documents = $2, updated_by = $3
WHERE id = $1
RETURNING *;

-- name: InsertReceivableAdjustment :exec
INSERT INTO operations.receivable_adjustment
    (company_id, merchant_id, receivable_id, kind, amount, reason, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7);

-- name: CreateClaimBatch :one
INSERT INTO operations.claim_batch
    (company_id, merchant_id, payer_id, kind, period_start, period_end, note,
     submission_deadline, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $9)
RETURNING *;

-- name: ListClaimBatchesByMerchant :many
SELECT cb.*, p.code AS payer_code, p.name AS payer_name
FROM operations.claim_batch cb
JOIN core.payer p ON p.id = cb.payer_id
WHERE cb.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.narg(payer_id)::uuid IS NULL OR cb.payer_id = sqlc.narg(payer_id))
  AND (sqlc.narg(status)::text IS NULL OR cb.status = sqlc.narg(status))
ORDER BY cb.period_end DESC, cb.id;

-- name: GetClaimBatchDetail :one
SELECT cb.*, p.code AS payer_code, p.name AS payer_name,
       p.payer_type, p.claim_documents AS payer_claim_documents
FROM operations.claim_batch cb
JOIN core.payer p ON p.id = cb.payer_id
WHERE cb.id = $1;

-- name: ListBatchCandidates :many
SELECT * FROM operations.receivable
WHERE merchant_id = sqlc.arg(merchant_id) AND payer_id = sqlc.arg(payer_id) AND status = 'open'
  AND service_date_to >= sqlc.arg(period_start) AND service_date_to <= sqlc.arg(period_end)
ORDER BY service_date_to, id;

-- name: ListReceivablesByIDs :many
SELECT * FROM operations.receivable WHERE id = ANY($1::uuid[]);

-- name: AttachReceivableToBatch :exec
UPDATE operations.receivable
SET batch_id = $2, status = 'in_batch', updated_by = $3
WHERE id = $1;

-- name: DetachReceivableFromBatch :exec
UPDATE operations.receivable
SET batch_id = NULL, status = 'open', updated_by = $2
WHERE id = $1;

-- name: RecomputeClaimBatchTotal :one
UPDATE operations.claim_batch cb
SET total_amount = COALESCE((
        SELECT sum(amount) FROM operations.receivable WHERE batch_id = cb.id
    ), 0), updated_by = $2
WHERE cb.id = $1
RETURNING *;

-- name: UpdateClaimBatchDocuments :one
UPDATE operations.claim_batch
SET documents = $2, updated_by = $3
WHERE id = $1
RETURNING *;

-- name: SubmitClaimBatch :one
UPDATE operations.claim_batch
SET status = 'submitted', submitted_at = now(), submitted_by = $2, updated_by = $2
WHERE id = $1
RETURNING *;

-- name: SetClaimBatchStatus :one
UPDATE operations.claim_batch
SET status = $2, updated_by = $3
WHERE id = $1
RETURNING *;

-- name: PromoteClaimBatchReceivables :exec
UPDATE operations.receivable
SET status = $2, updated_by = $3
WHERE batch_id = $1;

-- name: ListBatchReceivables :many
SELECT r.*, p.name AS payer_name, p.payer_type,
       a.visit_no AS admission_visit_no,
       per.id AS person_id, per.full_name AS person_full_name,
       per.medical_record_no AS person_mrn
FROM operations.receivable r
JOIN core.payer p ON p.id = r.payer_id
JOIN operations.admission a ON a.id = r.admission_id
JOIN core.person per ON per.id = a.person_id
WHERE r.batch_id = $1
ORDER BY r.service_date_to, r.id;

-- name: ClaimBatchExportRows :many
-- One CSV row per payer-side charge split (spec §4.2.6): party `payer` with a
-- positive amount for the batch payer; nomor_peserta comes from the visit's
-- guarantor coverage membership (empty when there is none).
SELECT per.full_name AS person_name,
       per.medical_record_no AS person_mrn,
       a.visit_no,
       c.service_date,
       si.code AS item_code,
       si.name AS item_name,
       c.quantity::text AS quantity_text,
       cs.claim_type,
       cs.amount,
       pc.member_number
FROM operations.receivable r
JOIN operations.admission a ON a.id = r.admission_id
JOIN core.person per ON per.id = a.person_id
JOIN operations.charge c
    ON c.account_id = r.account_id AND c.admission_id = r.admission_id AND c.status = 'active'
JOIN operations.charge_split cs ON cs.charge_id = c.id
JOIN core.service_item si ON si.id = c.service_item_id
LEFT JOIN operations.admission_guarantor g
    ON g.admission_id = r.admission_id AND g.payer_id = r.payer_id
LEFT JOIN core.patient_coverage pc ON pc.id = g.coverage_id
WHERE r.batch_id = $1
  AND cs.party = 'payer' AND cs.payer_id = r.payer_id AND cs.amount > 0
ORDER BY a.visit_no, c.service_date, c.id, cs.seq;
