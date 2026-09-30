-- internal/db/queries/audit_access_log.sql
-- core.audit_log, core.access_log (docs/07-core-ddl.md §10 "Audit log & Access log";
-- K1 audit trail: docs/design/specs/2026-09-29-audit-trail-design.md).
-- audit_log rows are written ONLY by the core.trg_audit_row() trigger (migration 000046).
-- Insert (access_log) + list only — rows are never updated or deleted by the app.

-- name: CreateAccessLog :exec
INSERT INTO core.access_log
    (company_id, merchant_id, actor_id, platform_admin_id, resource, resource_id, action, status_code, ip_address, person_id)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10);

-- name: ListAccessLogs :many
SELECT l.id, l.merchant_id, l.actor_id, l.platform_admin_id, l.resource, l.resource_id, l.action, l.status_code, l.ip_address, l.created_at,
       l.person_id, ap.full_name AS actor_name, pp.full_name AS patient_name, pp.medical_record_no AS patient_mrn
FROM core.access_log l
LEFT JOIN core.app_user u ON u.id = l.actor_id
LEFT JOIN core.person ap ON ap.id = u.person_id
LEFT JOIN core.person pp ON pp.id = l.person_id
WHERE l.company_id = sqlc.arg(company_id)  -- explicit, not only RLS (tests and owner roles bypass RLS)
  AND l.created_at >= sqlc.arg(from_time) AND l.created_at < sqlc.arg(to_time)
  AND (sqlc.narg(actor_id)::uuid IS NULL OR l.actor_id = sqlc.narg(actor_id))
  AND (sqlc.narg(resource)::text IS NULL OR l.resource = sqlc.narg(resource))
  AND (sqlc.narg(resource_id)::uuid IS NULL OR l.resource_id = sqlc.narg(resource_id))
  AND (sqlc.narg(person_id)::uuid IS NULL OR l.person_id = sqlc.narg(person_id))
  AND (sqlc.narg(action)::text IS NULL OR l.action = sqlc.narg(action))
ORDER BY l.created_at DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- name: ListChangeLogs :many
SELECT l.id, l.merchant_id, l.table_name, l.record_id, l.action, l.changed_fields, l.changed_by, l.platform_admin_id, l.changed_at,
       l.person_id, cp.full_name AS changed_by_name, pp.full_name AS patient_name, pp.medical_record_no AS patient_mrn
FROM core.audit_log l
LEFT JOIN core.app_user u ON u.id = l.changed_by
LEFT JOIN core.person cp ON cp.id = u.person_id
LEFT JOIN core.person pp ON pp.id = l.person_id
WHERE l.company_id = sqlc.arg(company_id)  -- explicit, not only RLS (tests and owner roles bypass RLS)
  AND l.changed_at >= sqlc.arg(from_time) AND l.changed_at < sqlc.arg(to_time)
  AND (sqlc.narg(table_name)::text IS NULL OR l.table_name = sqlc.narg(table_name))
  AND (sqlc.narg(record_id)::uuid IS NULL OR l.record_id = sqlc.narg(record_id))
  AND (sqlc.narg(changed_by)::uuid IS NULL OR l.changed_by = sqlc.narg(changed_by))
  AND (sqlc.narg(person_id)::uuid IS NULL OR l.person_id = sqlc.narg(person_id))
  AND (sqlc.narg(action)::text IS NULL OR l.action = sqlc.narg(action))
ORDER BY l.changed_at DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);

-- Search lookups that feed the person/user filter dropdowns in the audit UI
-- (spec §4.3). Soft-deleted rows stay searchable so old log rows stay readable;
-- the handler escapes LIKE wildcards in the pattern itself.

-- name: SearchAuditPatients :many
SELECT id, full_name, medical_record_no, birth_date, (deleted_at IS NOT NULL) AS deleted
FROM core.person
WHERE company_id = @company_id
  AND (full_name ILIKE '%' || @pattern::text || '%' OR medical_record_no = @q::text)
ORDER BY full_name
LIMIT 20;

-- name: SearchAuditUsers :many
SELECT u.id, u.username, p.full_name, u.is_active, (u.deleted_at IS NOT NULL) AS deleted
FROM core.app_user u
LEFT JOIN core.person p ON p.id = u.person_id
WHERE u.company_id = @company_id
  AND (u.username ILIKE '%' || @pattern::text || '%' OR p.full_name ILIKE '%' || @pattern::text || '%')
ORDER BY u.username
LIMIT 20;
