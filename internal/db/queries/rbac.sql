-- internal/db/queries/rbac.sql
-- core.app_user, core.permission, core.role, core.role_permission, core.user_merchant_role
-- (docs/07-core-ddl.md §2 "RBAC")

-- name: CreateAppUser :one
INSERT INTO core.app_user (company_id, person_id, username, email, phone, password_hash, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING *;

-- name: GetAppUserByID :one
SELECT * FROM core.app_user WHERE id = $1 AND deleted_at IS NULL;

-- name: GetAppUserByUsername :one
SELECT * FROM core.app_user WHERE company_id = $1 AND username = $2 AND deleted_at IS NULL;

-- name: ListAppUsersByCompany :many
SELECT * FROM core.app_user
WHERE company_id = $1 AND deleted_at IS NULL
ORDER BY username
LIMIT $2 OFFSET $3;

-- name: UpdateAppUser :one
UPDATE core.app_user
SET person_id = $2, email = $3, is_active = $4, updated_by = $5
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: UpdateAppUserPassword :exec
UPDATE core.app_user
SET password_hash = $2, updated_by = $3
WHERE id = $1 AND deleted_at IS NULL;

-- name: TouchAppUserLastLogin :exec
UPDATE core.app_user
SET last_login_at = now()
WHERE id = $1 AND deleted_at IS NULL;

-- name: SetAppUserMFASecret :exec
UPDATE core.app_user
SET mfa_secret = $2, updated_by = $3
WHERE id = $1 AND deleted_at IS NULL;

-- name: ClearAppUserMFASecret :exec
UPDATE core.app_user
SET mfa_secret = NULL, updated_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: SetAppUserPin :exec
UPDATE core.app_user
SET pin_hash = $2, updated_by = $3
WHERE id = $1 AND deleted_at IS NULL;

-- name: ClearAppUserPin :exec
UPDATE core.app_user
SET pin_hash = NULL, updated_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteAppUser :exec
UPDATE core.app_user
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePermission :one
INSERT INTO core.permission (code, description, module)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetPermissionByCode :one
SELECT * FROM core.permission WHERE code = $1;

-- name: ListPermissions :many
SELECT * FROM core.permission ORDER BY module, code;

-- name: UpdatePermission :one
UPDATE core.permission
SET description = $2, module = $3
WHERE id = $1
RETURNING *;

-- name: DeletePermission :exec
DELETE FROM core.permission WHERE id = $1;

-- name: CreateRole :one
INSERT INTO core.role (company_id, name, description, is_system, requires_physician_data, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $6)
RETURNING *;

-- name: GetRoleByID :one
SELECT * FROM core.role WHERE id = $1 AND deleted_at IS NULL;

-- name: ListRolesByCompany :many
SELECT * FROM core.role
WHERE company_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: UpdateRole :one
UPDATE core.role
SET name = $2, description = $3, requires_physician_data = $4, updated_by = $5
WHERE id = $1 AND deleted_at IS NULL AND NOT is_system
RETURNING *;

-- name: SoftDeleteRole :exec
UPDATE core.role
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL AND NOT is_system;

-- name: AddRolePermission :exec
INSERT INTO core.role_permission (role_id, permission_id)
VALUES ($1, $2)
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- name: RemoveRolePermission :exec
DELETE FROM core.role_permission WHERE role_id = $1 AND permission_id = $2;

-- name: ListPermissionsByRole :many
SELECT p.* FROM core.permission p
JOIN core.role_permission rp ON rp.permission_id = p.id
WHERE rp.role_id = $1
ORDER BY p.module, p.code;

-- name: AddUserMerchantRole :one
INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id, created_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, merchant_id, role_id) DO NOTHING
RETURNING *;

-- name: GetUserMerchantRoleByID :one
SELECT * FROM core.user_merchant_role WHERE id = $1;

-- name: RemoveUserMerchantRole :exec
DELETE FROM core.user_merchant_role WHERE id = $1;

-- name: ListRolesByUserMerchant :many
SELECT r.* FROM core.role r
JOIN core.user_merchant_role umr ON umr.role_id = r.id
WHERE umr.user_id = $1 AND umr.merchant_id = $2 AND r.deleted_at IS NULL;

-- name: ListUserMerchantRolesByUser :many
SELECT * FROM core.user_merchant_role WHERE user_id = $1;

-- name: UserHasPermission :one
SELECT EXISTS (
    SELECT 1
    FROM core.user_merchant_role umr
    JOIN core.role r ON r.id = umr.role_id AND r.deleted_at IS NULL
    JOIN core.role_permission rp ON rp.role_id = r.id
    JOIN core.permission p ON p.id = rp.permission_id
    WHERE umr.user_id = $1 AND umr.merchant_id = $2 AND p.code = $3 AND umr.role_id = $4
) AS has_permission;

-- name: ListPermissionCodesByUserMerchant :many
-- Effective permission codes for a user at a merchant (union across all
-- their roles there) -- self-access endpoint for permission-driven frontend
-- UI (nav/dashboard gating), since ListRolePermissions itself requires
-- PermRoleManage and a regular user can't read their own role's permission
-- list through it.
SELECT DISTINCT p.code
FROM core.user_merchant_role umr
JOIN core.role r ON r.id = umr.role_id AND r.deleted_at IS NULL
JOIN core.role_permission rp ON rp.role_id = r.id
JOIN core.permission p ON p.id = rp.permission_id
WHERE umr.user_id = $1 AND umr.merchant_id = $2
ORDER BY p.code;

-- name: StartAppUserMFAGrace :one
-- Starts the MFA grace window once (spec 2026-09-25-mfa-grace-enforcement §3).
-- Returns no row if it was already started (concurrent login) — caller re-reads.
UPDATE core.app_user
SET mfa_grace_until = now() + make_interval(days => sqlc.arg(grace_days)::int)
WHERE id = sqlc.arg(id) AND mfa_grace_until IS NULL AND deleted_at IS NULL
RETURNING mfa_grace_until;

-- name: RoleHasAnyPermission :one
-- Company-level check scoped to the active role (spec 2026-10-01-active-role-scope-design §3.4).
SELECT EXISTS (
    SELECT 1 FROM core.role_permission rp
    JOIN core.permission p ON p.id = rp.permission_id
    WHERE rp.role_id = @role_id AND p.code = ANY(@codes::text[])
) AS has_permission;

-- name: ListPermissionCodesByRole :many
SELECT p.code
FROM core.role_permission rp
JOIN core.permission p ON p.id = rp.permission_id
WHERE rp.role_id = @role_id
ORDER BY p.code;

-- name: UpdateOwnAppUserContact :one
-- Self-service profile edit (spec 2026-09-25-account-menu §3). NULL clears a value.
UPDATE core.app_user
SET email = sqlc.narg(email), phone = sqlc.narg(phone), updated_by = sqlc.arg(id)
WHERE id = sqlc.arg(id) AND deleted_at IS NULL
RETURNING *;
