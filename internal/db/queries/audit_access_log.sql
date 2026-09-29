-- internal/db/queries/audit_access_log.sql
-- core.audit_log, core.access_log (docs/07-core-ddl.md §10 "Audit log & Access log";
-- K1 audit trail: docs/design/specs/2026-09-29-audit-trail-design.md).
-- audit_log rows are written ONLY by the core.trg_audit_row() trigger (migration 000046).
-- Insert (access_log) + list only — rows are never updated or deleted by the app.

-- name: CreateAccessLog :exec
INSERT INTO core.access_log
    (company_id, merchant_id, actor_id, platform_admin_id, resource, resource_id, action, status_code, ip_address)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9);

-- name: ListAccessLogs :many
SELECT id, merchant_id, actor_id, platform_admin_id, resource, resource_id, action, status_code, ip_address, created_at
FROM core.access_log
WHERE company_id = sqlc.arg(company_id)  -- explicit, not only RLS (tests and owner roles bypass RLS)
  AND created_at >= sqlc.arg(from_time) AND created_at < sqlc.arg(to_time)
  AND (sqlc.narg(actor_id)::uuid IS NULL OR actor_id = sqlc.narg(actor_id))
  AND (sqlc.narg(resource)::text IS NULL OR resource = sqlc.narg(resource))
  AND (sqlc.narg(resource_id)::uuid IS NULL OR resource_id = sqlc.narg(resource_id))
ORDER BY created_at DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: ListChangeLogs :many
SELECT id, merchant_id, table_name, record_id, action, changed_fields, changed_by, platform_admin_id, changed_at
FROM core.audit_log
WHERE company_id = sqlc.arg(company_id)  -- explicit, not only RLS (tests and owner roles bypass RLS)
  AND changed_at >= sqlc.arg(from_time) AND changed_at < sqlc.arg(to_time)
  AND (sqlc.narg(table_name)::text IS NULL OR table_name = sqlc.narg(table_name))
  AND (sqlc.narg(record_id)::uuid IS NULL OR record_id = sqlc.narg(record_id))
  AND (sqlc.narg(changed_by)::uuid IS NULL OR changed_by = sqlc.narg(changed_by))
ORDER BY changed_at DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);
