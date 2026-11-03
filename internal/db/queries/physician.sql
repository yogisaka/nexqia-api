-- internal/db/queries/physician.sql
-- core.physician (docs/07-core-ddl.md §5 "Physician")

-- name: CreatePhysician :one
INSERT INTO core.physician (company_id, merchant_id, person_id, str_number, sip_number, sip_valid_until, specialty_concept_id, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
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
  AND EXISTS (
      SELECT 1
      FROM core.app_user u
      JOIN core.user_merchant_role umr ON umr.user_id = u.id
      JOIN core.role r ON r.id = umr.role_id
      WHERE u.person_id = p.person_id
        AND u.deleted_at IS NULL
        AND umr.merchant_id = p.merchant_id
        AND r.requires_physician_data
        AND r.deleted_at IS NULL)
ORDER BY pe.full_name
LIMIT $2 OFFSET $3;

-- name: UpdatePhysician :one
UPDATE core.physician
SET str_number = $2, sip_number = $3, sip_valid_until = $4, specialty_concept_id = $5, is_active = $6, updated_by = $7
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePhysician :exec
UPDATE core.physician
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: SoftDeletePhysicianIfUnclaimed :execrows
-- Soft-delete physician (person, merchant) bila TIDAK ada user aktif lain
-- (atau user sama dengan assignment role dokter lain) yang masih
-- mengklaim data physician ini. 0 baris = sah (tidak ada / masih diklaim).
UPDATE core.physician p
SET deleted_at = now(), deleted_by = $3
WHERE p.person_id = $1 AND p.merchant_id = $2 AND p.deleted_at IS NULL
  AND NOT EXISTS (
      SELECT 1
      FROM core.app_user u
      JOIN core.user_merchant_role umr ON umr.user_id = u.id
      JOIN core.role r ON r.id = umr.role_id
      WHERE u.person_id = $1
        AND u.deleted_at IS NULL
        AND umr.merchant_id = $2
        AND r.requires_physician_data
        AND r.deleted_at IS NULL);
