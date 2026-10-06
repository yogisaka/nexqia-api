-- internal/db/queries/mrn_config.sql
-- Configurable medical_record_no format (migrations/000032). One company-level
-- default row (merchant_id IS NULL) plus an optional per-merchant override.

-- name: GetMRNFormat :one
-- Merchant override first (if $1 matches a row), else the company default.
-- Returns no rows if neither exists — caller falls back to a hardcoded default.
-- Table aliases + qualified columns: sqlc's analyzer resolves both UNION
-- branches against a shared scope, so unqualified columns read as ambiguous.
(SELECT mc.format FROM core.mrn_config mc WHERE mc.merchant_id = $1)
UNION ALL
(SELECT mc2.format FROM core.mrn_config mc2 WHERE mc2.company_id = $2 AND mc2.merchant_id IS NULL)
LIMIT 1;

-- name: LockSequence :exec
-- Transaction-scoped advisory lock on (owner id, key) that serializes
-- "count then insert" sequence generators: MRN (company id, resolved
-- prefix|suffix) and the daily visit/queue numbers (merchant id,
-- "<scope>:<local date>", spec 2026-10-06-daily-numbering §3.2). Released
-- automatically at commit/rollback of the caller's request transaction.
SELECT pg_advisory_xact_lock(hashtext($1::text || ':' || $2::text));

-- name: TransactionTime :one
-- now() = start of the current transaction — the instant admission_at /
-- queue.created_at defaults record. Daily sequences derive their local day
-- from it, so a number and its row's timestamp fall on the same day.
SELECT now()::timestamptz AS tx_time;

-- name: MaxPersonMRNSeqAt :one
-- seq_start/seq_width = 1-based start position and digit width of the {SEQ:N}
-- slot inside medical_record_no once the format's other tokens are resolved;
-- like_pattern = LIKE pattern (literal parts escaped, {SEQ:N} slot as N '_'
-- wildcards).
SELECT COALESCE(MAX(SUBSTRING(medical_record_no FROM sqlc.arg(seq_start)::int FOR sqlc.arg(seq_width)::int)::int), 0)::int AS max_seq
FROM core.person
WHERE company_id = sqlc.arg(company_id) AND medical_record_no LIKE sqlc.arg(like_pattern)::text ESCAPE '\' AND deleted_at IS NULL;

-- name: GetCompanyMRNConfig :one
SELECT * FROM core.mrn_config WHERE company_id = $1 AND merchant_id IS NULL;

-- name: GetMerchantMRNConfig :one
SELECT * FROM core.mrn_config WHERE merchant_id = $1;

-- name: UpsertCompanyMRNConfig :one
INSERT INTO core.mrn_config (company_id, merchant_id, format, created_by, updated_by)
VALUES ($1, NULL, $2, $3, $3)
ON CONFLICT (company_id) WHERE merchant_id IS NULL
DO UPDATE SET format = EXCLUDED.format, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: UpsertMerchantMRNConfig :one
INSERT INTO core.mrn_config (company_id, merchant_id, format, created_by, updated_by)
VALUES ($1, $2, $3, $4, $4)
ON CONFLICT (merchant_id) WHERE merchant_id IS NOT NULL
DO UPDATE SET format = EXCLUDED.format, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: DeleteMerchantMRNConfig :exec
DELETE FROM core.mrn_config WHERE merchant_id = $1;
