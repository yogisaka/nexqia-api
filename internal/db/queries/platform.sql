-- internal/db/queries/platform.sql
-- platform.admin_user, platform.role, platform.permission, platform.role_permission,
-- platform.admin_user_role, platform.admin_refresh_token
-- (docs/design/specs/2026-09-24-platform-admin-foundation-design.md §3)

-- name: CreatePlatformAdminUser :one
INSERT INTO platform.admin_user (username, email, full_name, password_hash, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $5)
RETURNING *;

-- name: GetPlatformAdminUserByUsername :one
SELECT * FROM platform.admin_user WHERE username = $1 AND deleted_at IS NULL;

-- name: GetPlatformAdminUserByID :one
SELECT * FROM platform.admin_user WHERE id = $1 AND deleted_at IS NULL AND is_active;

-- name: TouchPlatformAdminUserLastLogin :exec
UPDATE platform.admin_user SET last_login_at = now() WHERE id = $1;

-- name: CreatePlatformAdminUserRole :exec
INSERT INTO platform.admin_user_role (admin_user_id, role_id)
VALUES ($1, $2)
ON CONFLICT (admin_user_id, role_id) DO NOTHING;

-- name: ListPlatformRoles :many
SELECT * FROM platform.role WHERE deleted_at IS NULL ORDER BY name;

-- name: AdminUserHasPlatformPermission :one
SELECT EXISTS (
    SELECT 1
    FROM platform.admin_user_role aur
    JOIN platform.role r ON r.id = aur.role_id AND r.deleted_at IS NULL
    JOIN platform.role_permission rp ON rp.role_id = r.id
    JOIN platform.permission p ON p.id = rp.permission_id
    WHERE aur.admin_user_id = $1 AND p.code = $2
) AS has_permission;

-- name: CreatePlatformAdminRefreshToken :one
INSERT INTO platform.admin_refresh_token
    (admin_user_id, device_id, device_label, token_hash, expires_at, created_ip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetPlatformAdminRefreshTokenByHash :one
SELECT * FROM platform.admin_refresh_token WHERE token_hash = $1;

-- name: RevokePlatformAdminRefreshToken :exec
UPDATE platform.admin_refresh_token SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeAllPlatformAdminRefreshTokensForDevice :exec
UPDATE platform.admin_refresh_token SET revoked_at = now()
WHERE admin_user_id = $1 AND device_id = $2 AND revoked_at IS NULL;
-- name: CreatePlatformImpersonationSession :one
INSERT INTO platform.impersonation_session
    (admin_user_id, target_user_id, target_company_id, reason, expires_at, started_ip)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;
