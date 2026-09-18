-- internal/db/queries/physician.sql
-- core.physician (docs/07-core-ddl.md §5 "Physician")

-- name: CreatePhysician :one
INSERT INTO core.physician (company_id, merchant_id, person_id, str_number, sip_number, specialty_concept_id, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING *;

-- name: GetPhysicianByID :one
SELECT * FROM core.physician WHERE id = $1 AND deleted_at IS NULL;

-- name: GetPhysicianByPerson :one
SELECT * FROM core.physician WHERE merchant_id = $1 AND person_id = $2 AND deleted_at IS NULL;

-- name: ListPhysiciansByMerchant :many
SELECT p.*,
       pe.full_name AS person_full_name,
       pe.photo_url AS person_photo_url,
       c.display    AS specialty_display
FROM core.physician p
JOIN core.person pe ON pe.id = p.person_id
LEFT JOIN terminology.concept c ON c.id = p.specialty_concept_id
WHERE p.merchant_id = $1 AND p.deleted_at IS NULL
ORDER BY pe.full_name
LIMIT $2 OFFSET $3;

-- name: UpdatePhysician :one
UPDATE core.physician
SET str_number = $2, sip_number = $3, specialty_concept_id = $4, is_active = $5, updated_by = $6
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePhysician :exec
UPDATE core.physician
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
