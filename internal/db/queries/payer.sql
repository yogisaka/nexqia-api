-- internal/db/queries/payer.sql
-- core.payer — payer/penjamin master (migration 000010). Merchant-scoped RLS;
-- list only (create/update not needed by the pendaftaran form, which selects
-- from seeded payers).

-- name: ListPayersByMerchant :many
SELECT * FROM core.payer
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;
