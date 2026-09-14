-- internal/db/queries/audit_access_log.sql
-- core.audit_log, core.access_log (docs/07-core-ddl.md §10 "Audit log & Access log")
-- Insert + list only — baris ini runtime-only, tidak pernah di-update/delete.

-- name: CreateAuditLog :one
INSERT INTO core.audit_log (company_id, merchant_id, table_name, record_id, action, old_value, new_value, changed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListAuditLogsByRecord :many
SELECT * FROM core.audit_log
WHERE table_name = $1 AND record_id = $2
ORDER BY changed_at DESC
LIMIT $3;

-- name: ListAuditLogsByCompany :many
SELECT * FROM core.audit_log
WHERE company_id = $1
ORDER BY changed_at DESC
LIMIT $2 OFFSET $3;

-- name: CreateAccessLog :one
INSERT INTO core.access_log (company_id, merchant_id, actor_id, resource, resource_id, action, ip_address)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: ListAccessLogsByActor :many
SELECT * FROM core.access_log
WHERE actor_id = $1
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: ListAccessLogsByResource :many
SELECT * FROM core.access_log
WHERE resource = $1 AND resource_id = $2
ORDER BY created_at DESC
LIMIT $3;
