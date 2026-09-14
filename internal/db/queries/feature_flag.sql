-- internal/db/queries/feature_flag.sql
-- core.feature_flag (docs/07-core-ddl.md §9 "Feature flag")

-- name: UpsertFeatureFlag :one
INSERT INTO core.feature_flag (merchant_id, flag_key, flag_value, created_by, updated_by)
VALUES ($1, $2, $3, $4, $4)
ON CONFLICT (merchant_id, flag_key) DO UPDATE
    SET flag_value = EXCLUDED.flag_value, updated_by = EXCLUDED.updated_by
RETURNING *;

-- name: GetFeatureFlag :one
SELECT * FROM core.feature_flag WHERE merchant_id = $1 AND flag_key = $2;

-- name: ListFeatureFlagsByMerchant :many
SELECT * FROM core.feature_flag WHERE merchant_id = $1 ORDER BY flag_key;

-- name: DeleteFeatureFlag :exec
DELETE FROM core.feature_flag WHERE merchant_id = $1 AND flag_key = $2;
