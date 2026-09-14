-- internal/db/queries/person.sql
-- core.person, core.patient_allergy, core.person_merge_log
-- (docs/07-core-ddl.md §4 "Person master")
-- Catatan: nik dienkripsi (pgp_sym_encrypt) di app layer sebelum masuk ke sini — kolom ini
-- hanya menyimpan/mengembalikan ciphertext base64, tidak pernah plaintext.

-- name: CreatePerson :one
INSERT INTO core.person (
    company_id, nik, medical_record_no, full_name, birth_date, birth_place, gender,
    blood_type, marital_status, religion_concept_id, education_concept_id, ethnicity_concept_id,
    phone, email, address, family_id, created_by, updated_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17, $17
)
RETURNING *;

-- name: GetPersonByID :one
SELECT * FROM core.person WHERE id = $1 AND deleted_at IS NULL;

-- name: GetPersonByMRN :one
SELECT * FROM core.person WHERE company_id = $1 AND medical_record_no = $2 AND deleted_at IS NULL;

-- name: ListPersonsByCompany :many
SELECT * FROM core.person
WHERE company_id = $1 AND deleted_at IS NULL
ORDER BY full_name
LIMIT $2 OFFSET $3;

-- name: SearchPersonsByName :many
SELECT * FROM core.person
WHERE company_id = $1 AND deleted_at IS NULL AND full_name ILIKE '%' || $2 || '%'
ORDER BY full_name
LIMIT $3;

-- name: UpdatePerson :one
UPDATE core.person
SET nik = $2, medical_record_no = $3, full_name = $4, birth_date = $5, birth_place = $6, gender = $7,
    blood_type = $8, marital_status = $9, religion_concept_id = $10, education_concept_id = $11,
    ethnicity_concept_id = $12, phone = $13, email = $14, address = $15, family_id = $16, updated_by = $17
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePerson :exec
UPDATE core.person
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePatientAllergy :one
INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name, reaction, severity, recorded_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetPatientAllergyByID :one
SELECT * FROM core.patient_allergy WHERE id = $1;

-- name: ListPatientAllergiesByPerson :many
SELECT * FROM core.patient_allergy
WHERE person_id = $1 AND is_active
ORDER BY recorded_at DESC;

-- name: UpdatePatientAllergy :one
UPDATE core.patient_allergy
SET reaction = $2, severity = $3, updated_by = $4
WHERE id = $1
RETURNING *;

-- name: DeactivatePatientAllergy :exec
UPDATE core.patient_allergy
SET is_active = false, updated_by = $2
WHERE id = $1;

-- name: CreatePersonMergeLog :one
INSERT INTO core.person_merge_log (surviving_person_id, merged_person_id, reason, merged_by)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: ListMergeLogsByPerson :many
SELECT * FROM core.person_merge_log
WHERE surviving_person_id = $1 OR merged_person_id = $1
ORDER BY merged_at DESC;
