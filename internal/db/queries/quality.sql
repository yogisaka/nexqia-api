-- internal/db/queries/quality.sql
-- Patient safety incident core API (spec
-- 2026-10-20-permenkes-6-2026-compliance-design §4 Fase B / §6): quality.incident
-- rows, append-only kronologi quality.incident_entry, and the data-driven
-- catalogs (NCC MERP gradings + 10 incident type categories, overridable per
-- merchant) — migration 000079.

-- name: CreateIncident :one
INSERT INTO quality.incident
    (company_id, merchant_id, incident_type, grading, occurred_at, location, narrative, patient_impact, person_id, admission_id, reported_by, anonymous)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING *;

-- name: CreateIncidentEntry :one
INSERT INTO quality.incident_entry
    (company_id, merchant_id, incident_id, kind, body, created_by)
VALUES ($1, $2, $3, $4, $5, $6)
RETURNING *;

-- name: GetIncidentByID :one
SELECT i.*, per.full_name AS reported_by_name
FROM quality.incident i
JOIN core.app_user au ON au.id = i.reported_by
LEFT JOIN core.person per ON per.id = au.person_id
WHERE i.id = $1;

-- name: ListIncidents :many
SELECT i.*, per.full_name AS reported_by_name
FROM quality.incident i
JOIN core.app_user au ON au.id = i.reported_by
LEFT JOIN core.person per ON per.id = au.person_id
WHERE i.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.narg(status)::text IS NULL OR i.status = sqlc.narg(status))
  AND (sqlc.narg(incident_type)::text IS NULL OR i.incident_type = sqlc.narg(incident_type))
  AND (sqlc.narg(grading)::text IS NULL OR i.grading = sqlc.narg(grading))
  AND (sqlc.narg(occurred_from)::timestamptz IS NULL OR i.occurred_at >= sqlc.narg(occurred_from))
  AND (sqlc.narg(occurred_to)::timestamptz IS NULL OR i.occurred_at < sqlc.narg(occurred_to))
ORDER BY i.occurred_at DESC, i.id DESC
LIMIT sqlc.arg(row_limit)::integer OFFSET sqlc.arg(row_offset)::integer;

-- name: CountIncidents :one
SELECT COUNT(*)::bigint
FROM quality.incident i
JOIN core.app_user au ON au.id = i.reported_by
LEFT JOIN core.person per ON per.id = au.person_id
WHERE i.merchant_id = sqlc.arg(merchant_id)
  AND (sqlc.narg(status)::text IS NULL OR i.status = sqlc.narg(status))
  AND (sqlc.narg(incident_type)::text IS NULL OR i.incident_type = sqlc.narg(incident_type))
  AND (sqlc.narg(grading)::text IS NULL OR i.grading = sqlc.narg(grading))
  AND (sqlc.narg(occurred_from)::timestamptz IS NULL OR i.occurred_at >= sqlc.narg(occurred_from))
  AND (sqlc.narg(occurred_to)::timestamptz IS NULL OR i.occurred_at < sqlc.narg(occurred_to));

-- name: ListIncidentEntries :many
SELECT e.*, per.full_name AS created_by_name
FROM quality.incident_entry e
JOIN core.app_user au ON au.id = e.created_by
LEFT JOIN core.person per ON per.id = au.person_id
WHERE e.incident_id = $1
ORDER BY e.created_at ASC, e.id ASC;

-- name: UpdateIncidentStatus :one
UPDATE quality.incident SET status = $2 WHERE id = $1 RETURNING *;

-- name: ListEffectiveIncidentTypes :many
SELECT t.code, COALESCE(o.name, t.name) AS name, COALESCE(o.is_active, true) AS is_active
FROM quality.incident_type t
LEFT JOIN quality.incident_type_override o ON o.incident_type = t.code AND o.merchant_id = $1
ORDER BY t.sort_order;

-- name: ListIncidentGradings :many
SELECT * FROM quality.incident_grading ORDER BY sort_order;

-- name: PersonBelongsToCompany :one
SELECT 1 FROM core.person WHERE id = $1 AND company_id = $2;

-- name: AdmissionBelongsToMerchant :one
SELECT 1 FROM operations.admission WHERE id = $1 AND merchant_id = $2;
