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

-- name: ListSecuritySettingLog :many
SELECT l.id, l.company_id, l.merchant_id, l.flag_key, l.old_value, l.new_value,
       l.changed_by, l.platform_admin_id, l.changed_at,
       p.full_name AS changed_by_name
FROM core.security_setting_log l
LEFT JOIN core.app_user u ON u.id = l.changed_by
LEFT JOIN core.person p ON p.id = u.person_id
WHERE l.merchant_id = @merchant_id
ORDER BY l.changed_at DESC, l.id DESC
LIMIT sqlc.arg(row_limit)
OFFSET sqlc.arg(row_offset);

-- name: SecurityUserStats :one
SELECT CAST(count(*) AS bigint) AS users_total,
       CAST(count(*) FILTER (WHERE u.pin_hash IS NULL) AS bigint) AS users_without_pin,
       CAST(count(*) FILTER (WHERE u.mfa_secret IS NULL) AS bigint) AS users_without_mfa
FROM core.app_user u
WHERE u.is_active AND u.deleted_at IS NULL
  AND (EXISTS (SELECT 1 FROM core.user_merchant_role mr WHERE mr.user_id = u.id AND mr.merchant_id = $1)
       OR EXISTS (SELECT 1 FROM core.user_company_role cr WHERE cr.user_id = u.id AND cr.company_id = $2));
