-- internal/db/queries/audit_access_log.sql
-- core.audit_log, core.access_log (docs/07-core-ddl.md §10 "Audit log & Access log";
-- K1 audit trail: 2026-09-29-audit-trail-design.md).
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

-- name: AccessReviewCounts :many
-- Per-actor activity counts for a review period (spec §6.1). Flags and the median are
-- computed in Go from these counts. Access counts exclude resource='audit' except
-- audit_actions. Local hour uses the configured audit time zone.
WITH rel AS (
    SELECT l.actor_id,
           l.resource,
           l.action,
           l.status_code,
           l.platform_admin_id,
           l.created_at,
           (p.id IS NOT NULL AND s.id IS NOT NULL AND (
                p.id = s.id OR p.family_id = s.id OR p.id = s.family_id
                OR (p.family_id IS NOT NULL AND p.family_id = s.family_id))) AS related
    FROM core.access_log l
    LEFT JOIN core.app_user u ON u.id = l.actor_id
    LEFT JOIN core.person s ON s.id = u.person_id
    LEFT JOIN core.person p ON p.id = l.person_id
    WHERE l.company_id = sqlc.arg(company_id)
      AND l.created_at >= sqlc.arg(from_time) AND l.created_at < sqlc.arg(to_time)
), acc AS (
    SELECT actor_id,
           count(*) FILTER (WHERE resource <> 'audit' AND action = 'view')                   AS views,
           count(*) FILTER (WHERE resource <> 'audit' AND action IN ('list', 'search'))      AS lists,
           count(*) FILTER (WHERE resource <> 'audit' AND status_code = 403)                 AS denied,
           count(*) FILTER (WHERE resource <> 'audit' AND (
               extract(hour FROM created_at AT TIME ZONE sqlc.arg(tz)::text) < sqlc.arg(start_hour)::int
               OR extract(hour FROM created_at AT TIME ZONE sqlc.arg(tz)::text) >= sqlc.arg(end_hour)::int)) AS after_hours,
           count(*) FILTER (WHERE resource <> 'audit' AND related)                           AS self_family_access,
           count(*) FILTER (WHERE resource <> 'audit' AND platform_admin_id IS NOT NULL)     AS platform_admin,
           count(*) FILTER (WHERE resource = 'audit')                                        AS audit_actions
    FROM rel GROUP BY actor_id
), chg AS (
    SELECT a.changed_by AS actor_id,
           count(*) AS changes,
           count(*) FILTER (WHERE p.id IS NOT NULL AND s.id IS NOT NULL AND (
                p.id = s.id OR p.family_id = s.id OR p.id = s.family_id
                OR (p.family_id IS NOT NULL AND p.family_id = s.family_id))) AS self_family_change
    FROM core.audit_log a
    LEFT JOIN core.app_user u ON u.id = a.changed_by
    LEFT JOIN core.person s ON s.id = u.person_id
    LEFT JOIN core.person p ON p.id = a.person_id
    WHERE a.company_id = sqlc.arg(company_id)
      AND a.changed_at >= sqlc.arg(from_time) AND a.changed_at < sqlc.arg(to_time)
      AND a.changed_by IS NOT NULL
    GROUP BY a.changed_by
)
SELECT COALESCE(acc.actor_id, chg.actor_id)::uuid                  AS actor_id,
       n.full_name                                                  AS actor_name,
       COALESCE(acc.views, 0)::bigint                               AS views,
       COALESCE(acc.lists, 0)::bigint                               AS lists,
       COALESCE(chg.changes, 0)::bigint                             AS changes,
       COALESCE(acc.denied, 0)::bigint                              AS denied,
       COALESCE(acc.after_hours, 0)::bigint                         AS after_hours,
       (COALESCE(acc.self_family_access, 0) + COALESCE(chg.self_family_change, 0))::bigint AS self_family,
       COALESCE(acc.platform_admin, 0)::bigint                      AS platform_admin,
       COALESCE(acc.audit_actions, 0)::bigint                       AS audit_actions
FROM acc
FULL OUTER JOIN chg ON chg.actor_id = acc.actor_id
LEFT JOIN core.app_user nu ON nu.id = COALESCE(acc.actor_id, chg.actor_id)
LEFT JOIN core.person n ON n.id = nu.person_id;

-- name: CreateAccessReview :one
INSERT INTO core.access_review
    (company_id, period_from, period_to, reviewed_by, flagged_users, notes)
VALUES (@company_id, @period_from, @period_to, @reviewed_by, @flagged_users, @notes)
RETURNING *;

-- name: ListAccessReviews :many
SELECT r.*, rp.full_name AS reviewed_by_name
FROM core.access_review r
LEFT JOIN core.app_user ru ON ru.id = r.reviewed_by
LEFT JOIN core.person rp ON rp.id = ru.person_id
WHERE r.company_id = @company_id  -- explicit, not only RLS (tests and owner roles bypass RLS)
ORDER BY r.reviewed_at DESC
LIMIT sqlc.arg(row_limit) OFFSET sqlc.arg(row_offset);
