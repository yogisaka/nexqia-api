-- internal/db/queries/person_document.sql
-- core.document_type, core.person_document, core.registration_document_requirement
-- (spec 2026-10-17-patient-documents-design §4–§5, migration 000073)

-- name: CreatePersonDocument :one
INSERT INTO core.person_document (
    company_id, merchant_id, person_id, media_id, document_type, label,
    issued_date, expired_date, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;

-- name: GetPersonDocumentByID :one
SELECT d.*, m.mime_type, m.size_bytes, m.original_filename, m.storage_key,
       m.virus_scan_status, dt.name AS document_type_name,
       COALESCE(pe.full_name, u.username) AS created_by_name
FROM core.person_document d
JOIN core.media m ON m.id = d.media_id
JOIN core.document_type dt ON dt.code = d.document_type
LEFT JOIN core.app_user u ON u.id = d.created_by
LEFT JOIN core.person pe ON pe.id = u.person_id
WHERE d.id = $1 AND d.deleted_at IS NULL;

-- name: ListPersonDocumentsByPerson :many
-- Merchant filter: isolation is enforced here (RLS alone is not enough for
-- roles that bypass it, and person is company-level while documents are
-- merchant-level, K11).
-- $3 (admission_id, nullable) filters via core.media_link (owner_table
-- 'admission'); NULL keeps the full person list unchanged. DISTINCT guards
-- against duplicating a document linked to several admissions.
SELECT DISTINCT d.*, m.mime_type, m.size_bytes, m.original_filename, m.virus_scan_status,
       dt.name AS document_type_name,
       COALESCE(pe.full_name, u.username) AS created_by_name
FROM core.person_document d
JOIN core.media m ON m.id = d.media_id
JOIN core.document_type dt ON dt.code = d.document_type
LEFT JOIN core.app_user u ON u.id = d.created_by
LEFT JOIN core.person pe ON pe.id = u.person_id
LEFT JOIN core.media_link ml ON ml.media_id = d.media_id
  AND ml.owner_table = 'admission' AND ml.deleted_at IS NULL
WHERE d.person_id = $1 AND d.merchant_id = $2 AND d.deleted_at IS NULL
  AND (sqlc.arg('admission_id')::uuid IS NULL OR ml.owner_id = sqlc.arg('admission_id')::uuid)
ORDER BY d.created_at DESC;

-- name: UpdatePersonDocument :one
UPDATE core.person_document
SET label = $2, issued_date = $3, expired_date = $4
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePersonDocument :exec
UPDATE core.person_document
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListDocumentRequirements :many
SELECT service_type, document_type
FROM core.registration_document_requirement
WHERE merchant_id = $1
  AND (sqlc.narg('service_type')::text IS NULL OR service_type = sqlc.arg('service_type'))
ORDER BY service_type, document_type;

-- name: ReplaceDocumentRequirements :exec
-- Insert the new requirement set; the caller (handler) deletes the old set in
-- the same transaction first (DeleteDocumentRequirementsByServiceType). The
-- values come from unnest alone so an empty existing set still inserts.
INSERT INTO core.registration_document_requirement (company_id, merchant_id, service_type, document_type)
SELECT $3, $1, $2, x
FROM unnest($4::text[]) AS x;

-- name: DeleteDocumentRequirementsByServiceType :exec
DELETE FROM core.registration_document_requirement
WHERE merchant_id = $1 AND service_type = $2;

-- name: ListDocumentTypes :many
SELECT code, name, sort_order
FROM core.document_type
ORDER BY sort_order;
