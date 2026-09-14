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
SELECT * FROM core.physician
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY created_at
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
