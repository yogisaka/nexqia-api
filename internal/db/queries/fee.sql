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

-- Fee payout batches and PPh 21 reference data
-- (spec 2026-10-07-tariff-d-practitioner-fees-tax §4.3, migration 000070).

-- name: GetFeeBatchByPeriod :one
SELECT id, status FROM operations.fee_payout_batch
WHERE merchant_id = $1 AND period_year = $2 AND period_month = $3;

-- name: CreateFeeBatch :one
INSERT INTO operations.fee_payout_batch (company_id, merchant_id, period_year, period_month, created_by)
VALUES ($1, $2, $3, $4, $5)
RETURNING *;

-- name: GetFeeBatchByID :one
SELECT * FROM operations.fee_payout_batch WHERE id = $1;

-- name: ListFeeBatchesByMerchant :many
SELECT * FROM operations.fee_payout_batch
WHERE merchant_id = $1 AND (sqlc.narg('year')::smallint IS NULL OR period_year = sqlc.narg('year')::smallint)
ORDER BY period_year DESC, period_month DESC;

-- name: SumOpenFeesByPhysician :many
SELECT pf.physician_id,
       sum(pf.gross_amount)::numeric(15,2) AS gross_amount,
       sum(pf.fee_amount)::numeric(15,2)   AS fee_amount,
       coalesce(max(ph.employment_type)::text, 'owner')::text AS employment_type,
       coalesce(max(ph.ptkp_status)::text, '')::text          AS ptkp_status
FROM operations.practitioner_fee pf
JOIN core.physician ph ON ph.id = pf.physician_id
WHERE pf.merchant_id = $1 AND pf.status = 'open' AND pf.service_date <= $2
GROUP BY pf.physician_id
ORDER BY pf.physician_id;

-- name: MarkFeesInBatch :exec
UPDATE operations.practitioner_fee SET batch_id = $2, status = 'in_batch'
WHERE merchant_id = $1 AND status = 'open' AND service_date <= $3;

-- name: ReleaseBatchFees :exec
UPDATE operations.practitioner_fee SET batch_id = NULL, status = 'open' WHERE batch_id = $1;

-- name: PayBatchFees :exec
UPDATE operations.practitioner_fee SET status = 'paid' WHERE batch_id = $1;

-- name: InsertFeePayoutLine :exec
INSERT INTO operations.fee_payout_line (company_id, merchant_id, batch_id, physician_id, employment_type, ptkp_status, gross_amount, fee_amount)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8);

-- name: ListFeePayoutLines :many
SELECT * FROM operations.fee_payout_line WHERE batch_id = $1 ORDER BY physician_id;

-- name: GetFeePayoutLine :one
SELECT * FROM operations.fee_payout_line WHERE batch_id = $1 AND physician_id = $2;

-- name: SetFeePayoutLineOtherGross :one
UPDATE operations.fee_payout_line SET other_gross = $3
WHERE batch_id = $1 AND physician_id = $2
RETURNING *;

-- name: UpdateFeePayoutLineTax :one
UPDATE operations.fee_payout_line
SET tax_method = $2, tax_base = $3, ter_category = $4, tax_rate = $5, tax_amount = $6, net_amount = $7
WHERE id = $1
RETURNING *;

-- name: LockFeeBatch :one
UPDATE operations.fee_payout_batch SET status = 'locked', locked_at = now(), locked_by = $2
WHERE id = $1
RETURNING *;

-- name: PayFeeBatch :one
UPDATE operations.fee_payout_batch SET status = 'paid', paid_at = now(), paid_by = $2
WHERE id = $1
RETURNING *;

-- name: DeleteFeeBatch :exec
DELETE FROM operations.fee_payout_batch WHERE id = $1;

-- name: ListFeePayoutLinesWithPerson :many
SELECT l.*, pe.full_name AS person_full_name, pe.nik, ph.npwp
FROM operations.fee_payout_line l
JOIN core.physician ph ON ph.id = l.physician_id
JOIN core.person pe ON pe.id = ph.person_id
WHERE l.batch_id = $1
ORDER BY l.physician_id;

-- name: ListFeePayoutYearLines :many
-- Locked/paid Jan–November lines of one physician in one year: the annual
-- December tax base (gross + other) and the tax already withheld.
SELECT l.gross_amount, l.other_gross, coalesce(l.tax_amount, 0) AS tax_amount
FROM operations.fee_payout_line l
JOIN operations.fee_payout_batch b ON b.id = l.batch_id
WHERE l.physician_id = $1 AND b.merchant_id = $2 AND b.period_year = $3
  AND b.period_month < 12 AND b.status IN ('locked','paid');

-- name: ListTaxTERCurrent :many
-- All TER rows of the latest table effective on the given date.
SELECT category, lower_exclusive, upper_inclusive, rate_percent
FROM core.tax_ter_rate
WHERE effective_from = (SELECT max(t.effective_from) FROM core.tax_ter_rate t WHERE t.effective_from <= $1)
ORDER BY category, lower_exclusive;

-- name: ListTaxPTKPCurrent :many
-- Latest PTKP annual amount per status effective on the given date.
SELECT DISTINCT ON (status) status, annual_amount
FROM core.tax_ptkp
WHERE effective_from <= $1
ORDER BY status, effective_from DESC;

-- name: ListTaxPasal17Current :many
-- All brackets of the latest Pasal 17 table effective on the given date.
SELECT upper_inclusive, rate_percent
FROM core.tax_pasal17_bracket
WHERE effective_from = (SELECT max(t.effective_from) FROM core.tax_pasal17_bracket t WHERE t.effective_from <= $1)
ORDER BY upper_inclusive NULLS LAST;

-- name: ListTaxSettingsCurrent :many
-- Latest value per tax setting key effective on the given date.
SELECT DISTINCT ON (key) key, value
FROM core.tax_setting
WHERE effective_from <= $1
ORDER BY key, effective_from DESC;
