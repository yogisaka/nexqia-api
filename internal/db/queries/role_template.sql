-- #7 role templates (spec 2026-09-30-role-templates-design §4). RLS filters
-- template scope; platform rows are visible to every tenant.

-- name: GetRoleTemplate :one
SELECT id, version FROM core.template WHERE id = @id AND kind = 'role' AND is_active;

-- name: ListRoleTemplates :many
SELECT id, code, name, description, version
FROM core.template
WHERE kind = 'role' AND is_active
ORDER BY name;

-- name: ListRoleTemplateRoles :many
-- INNER JOIN on permissions is deliberate: every template role has >= 1
-- permission (enforced by the 000051 seed DO block).
SELECT tr.id, tr.template_id, tr.name, tr.description, tr.requires_physician_data, tr.sort_order,
       p.id AS permission_id, p.code AS permission_code, p.description AS permission_description
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id
JOIN core.template_role_permission trp ON trp.template_role_id = tr.id
JOIN core.permission p ON p.id = trp.permission_id
WHERE t.kind = 'role' AND t.is_active
ORDER BY tr.template_id, tr.sort_order, p.code;

-- name: ListCompanyRoleNames :many
SELECT name, (deleted_at IS NOT NULL)::boolean AS deleted FROM core.role WHERE company_id = @company_id;

-- name: CreateTemplateApplication :exec
INSERT INTO core.template_application
    (company_id, template_id, template_version, applied_by, platform_admin_id, roles_created, roles_skipped)
VALUES (@company_id, @template_id, @template_version, @applied_by, @platform_admin_id, @roles_created, @roles_skipped);
