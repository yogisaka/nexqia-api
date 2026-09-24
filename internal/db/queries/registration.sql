-- internal/db/queries/registration.sql
-- New queries for POST /auth/register (see docs/design/specs/2026-09-23-saas-registration-owner-bootstrap-design.md §6).

-- name: CreateCompanyWithID :one
-- Explicit id (not DB DEFAULT) — company_isolation RLS has no WITH CHECK, so
-- USING doubles as the insert check: the new row's id must already equal
-- app.current_company_id, which must be set to a value we chose BEFORE this
-- insert runs. See §6 poin 4-6 for why.
INSERT INTO core.company (id, code, name, created_by, updated_by)
VALUES ($1, $2, $3, $4, $4)
RETURNING *;

-- name: CheckAppUserEmailExists :one
SELECT EXISTS (SELECT 1 FROM core.app_user WHERE email = $1 AND deleted_at IS NULL);

-- name: CheckAppUserPhoneExists :one
SELECT EXISTS (SELECT 1 FROM core.app_user WHERE phone = $1 AND deleted_at IS NULL);

-- name: CreateUserCompanyRole :one
INSERT INTO core.user_company_role (user_id, company_id, role_id, created_by)
VALUES ($1, $2, $3, $4)
ON CONFLICT (user_id, company_id, role_id) DO NOTHING
RETURNING *;

-- name: UserHasCompanyLevelPermission :one
SELECT core.user_has_company_level_permission($1, $2, $3);
