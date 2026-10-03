-- #26 department templates + external code mapping (spec
-- 2026-10-01-department-templates-design). RLS filters template scope; platform
-- rows are visible to every tenant. department_code_map is RLS-scoped per
-- merchant (app.current_merchant_id).

-- name: GetDepartmentTemplate :one
SELECT id, version FROM core.template WHERE id = @id AND kind = 'department' AND is_active;

-- name: ListDepartmentTemplates :many
SELECT id, code, name, description, version
FROM core.template
WHERE kind = 'department' AND is_active
ORDER BY name;

-- name: ListTemplateDepartments :many
SELECT td.id, td.template_id, td.code, td.name, td.specialty_code
FROM core.template_department td
JOIN core.template t ON t.id = td.template_id
WHERE t.kind = 'department' AND t.is_active
ORDER BY td.template_id, td.sort_order;

-- name: ListTemplateDepartmentCodeMaps :many
SELECT m.template_department_id, m.system, m.code
FROM core.template_department_code_map m
JOIN core.template_department td ON td.id = m.template_department_id
JOIN core.template t ON t.id = td.template_id
WHERE t.kind = 'department' AND t.is_active
ORDER BY m.template_department_id, m.system;

-- name: ListDepartmentNamesCodesByMerchant :many
-- Includes soft-deleted departments: a deleted poli with the same code/name
-- still blocks a template department ("deleted_exists"), it must never 500.
SELECT code, name, (deleted_at IS NOT NULL)::boolean AS deleted
FROM core.department
WHERE merchant_id = @merchant_id;

-- name: GetConceptIDByCode :one
-- SPEC-* codes are unique per code system; ORDER BY + LIMIT 1 keeps the pick
-- deterministic if the same code exists in several code systems.
SELECT id FROM terminology.concept WHERE code = @code ORDER BY id LIMIT 1;

-- name: ListDepartmentCodeMaps :many
SELECT system, code, display FROM core.department_code_map WHERE department_id = @department_id ORDER BY system;

-- name: DeleteDepartmentCodeMapsNotIn :exec
DELETE FROM core.department_code_map
WHERE department_id = @department_id AND system <> ALL(@systems::text[]);

-- name: UpsertDepartmentCodeMap :exec
INSERT INTO core.department_code_map (department_id, system, code, display, created_by, updated_by)
VALUES (@department_id, @system, @code, @display, @actor_id, @actor_id)
ON CONFLICT (department_id, system)
DO UPDATE SET code = EXCLUDED.code, display = EXCLUDED.display, updated_by = EXCLUDED.updated_by;

-- name: CreateDepartmentCodeMap :exec
INSERT INTO core.department_code_map (department_id, system, code, display, created_by, updated_by)
VALUES (@department_id, @system, @code, @display, @actor_id, @actor_id);
