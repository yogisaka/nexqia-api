-- internal/db/queries/person_family.sql
-- core.person_family — family member master (migration 000028,
-- 2026-09-18-fo-pendaftaran-full-design.md §4.2).
-- Company-scoped: semua query filter deleted_at IS NULL.

-- name: ListPersonFamiliesByPerson :many
SELECT * FROM core.person_family
WHERE person_id = $1 AND deleted_at IS NULL
ORDER BY created_at;

-- name: GetPersonFamilyByID :one
SELECT * FROM core.person_family
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePersonFamily :one
INSERT INTO core.person_family (
    company_id, person_id, full_name, phone, email, birth_date, birth_place, gender,
    address, region_village_concept_id, postal_code, relationship_concept_id,
    position_concept_id, job_concept_id, is_responsible_person, created_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16
)
RETURNING *;

-- name: UpdatePersonFamily :one
UPDATE core.person_family
SET full_name = $2, phone = $3, email = $4, birth_date = $5, birth_place = $6, gender = $7,
    address = $8, region_village_concept_id = $9, postal_code = $10, relationship_concept_id = $11,
    position_concept_id = $12, job_concept_id = $13, is_responsible_person = $14, updated_by = $15
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePersonFamily :exec
UPDATE core.person_family
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
