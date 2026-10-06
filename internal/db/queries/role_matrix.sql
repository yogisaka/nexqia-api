-- name: ListRolesWithCounts :many
SELECT r.id, r.company_id, r.name, r.description, r.is_system, r.requires_physician_data,
       COALESCE(p.perm_count, 0)::bigint AS permission_count,
       COALESCE(u.user_count, 0)::bigint AS user_count
FROM core.role r
LEFT JOIN (
    SELECT role_id, COUNT(*) AS perm_count FROM core.role_permission GROUP BY role_id
) p ON p.role_id = r.id
LEFT JOIN (
    SELECT role_id, COUNT(DISTINCT user_id) AS user_count
    FROM core.user_merchant_role GROUP BY role_id
) u ON u.role_id = r.id
WHERE r.company_id = $1 AND r.deleted_at IS NULL
ORDER BY r.name
LIMIT $2 OFFSET $3;

-- name: ListRoleWidgets :many
SELECT role_id, widget_key, visible
FROM core.role_widget
WHERE role_id = $1;

-- name: DeleteRoleWidgets :execrows
DELETE FROM core.role_widget WHERE role_id = $1;

-- name: AddRoleWidget :exec
INSERT INTO core.role_widget (role_id, widget_key, visible) VALUES ($1, $2, $3);

-- name: DeleteRolePermissionsNotIn :execrows
DELETE FROM core.role_permission WHERE role_id = $1 AND NOT (permission_id = ANY($2::uuid[]));

-- name: InsertRoleChangeLog :exec
INSERT INTO core.role_change_log (company_id, role_id, changed_by, reason, changes)
VALUES ($1, $2, $3, $4, $5);

-- name: ListRoleChanges :many
SELECT l.changed_at, l.reason, l.changes, p.full_name AS changed_by_name
FROM core.role_change_log l
LEFT JOIN core.app_user u ON u.id = l.changed_by
LEFT JOIN core.person p ON p.id = u.person_id
WHERE l.role_id = $1
ORDER BY l.changed_at DESC
LIMIT $2;

-- name: GetLastRoleChange :one
SELECT l.changed_at, l.reason, l.changes, p.full_name AS changed_by_name
FROM core.role_change_log l
LEFT JOIN core.app_user u ON u.id = l.changed_by
LEFT JOIN core.person p ON p.id = u.person_id
WHERE l.role_id = $1
ORDER BY l.changed_at DESC
LIMIT 1;

-- name: CopyRolePermissions :execrows
INSERT INTO core.role_permission (role_id, permission_id)
SELECT $1, permission_id FROM core.role_permission src WHERE src.role_id = $2;

-- name: CopyRoleWidgets :execrows
INSERT INTO core.role_widget (role_id, widget_key, visible)
SELECT $1, widget_key, visible FROM core.role_widget src WHERE src.role_id = $2;

-- name: ListPermissionCodesByIDs :many
SELECT code FROM core.permission WHERE id = ANY($1::uuid[]);

-- name: TouchRoleForMatrixSave :one
-- Optimistic-lock write of PUT /roles/:id/matrix: metadata + row touch
-- (trg_touch_row bumps row_version). No "AND NOT is_system" (unlike
-- UpdateRole): system roles are touched with their metadata unchanged — the
-- handler rejects metadata changes on them. 0 rows = stale row_version.
UPDATE core.role
SET name = sqlc.arg('name'), description = sqlc.arg('description'),
    requires_physician_data = sqlc.arg('requires_physician_data'), updated_by = sqlc.arg('updated_by')
WHERE id = sqlc.arg('id') AND row_version = sqlc.arg('row_version') AND deleted_at IS NULL
RETURNING *;
