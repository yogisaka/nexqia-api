-- internal/db/queries/person.sql
-- core.person, core.patient_allergy, core.person_merge_log
-- (docs/07-core-ddl.md §4 "Person master")
-- Catatan: nik dienkripsi (pgp_sym_encrypt) di app layer sebelum masuk ke sini — kolom ini
-- hanya menyimpan/mengembalikan ciphertext base64, tidak pernah plaintext.

-- name: CreatePerson :one
-- Param order: $1 company_id, $2 nik, $3 medical_record_no, $4 full_name,
-- $5 birth_date, $6 birth_place, $7 gender, $8 blood_type, $9 marital_status,
-- $10 religion_concept_id, $11 education_concept_id, $12 ethnicity_concept_id,
-- $13 phone, $14 email, $15 address, $16 family_id,
-- $17 region_village_concept_id, $18 citizenship_concept_id,
-- $19 identity_type_concept_id, $20 job_concept_id,
-- $21 marital_status_concept_id, $22 blood_type_concept_id,
-- $23 nik_search_hash, $24 created_by/updated_by.
INSERT INTO core.person (
    company_id, nik, medical_record_no, full_name, birth_date, birth_place, gender,
    blood_type, marital_status, religion_concept_id, education_concept_id, ethnicity_concept_id,
    phone, email, address, family_id,
    region_village_concept_id, citizenship_concept_id, identity_type_concept_id,
    job_concept_id, marital_status_concept_id, blood_type_concept_id,
    nik_search_hash, created_by, updated_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16,
    $17, $18, $19, $20, $21, $22, $23, $24, $24
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
WHERE company_id = $1 AND deleted_at IS NULL AND (
    full_name ILIKE '%' || $2 || '%'
    OR medical_record_no ILIKE '%' || $2 || '%'
    OR phone ILIKE '%' || $2 || '%'
    OR nik_search_hash = $3
)
ORDER BY full_name
LIMIT $4;

-- name: UpdatePerson :one
-- Param order: $1 id, $2 nik, $3 medical_record_no, $4 full_name,
-- $5 birth_date, $6 birth_place, $7 gender, $8 blood_type, $9 marital_status,
-- $10 religion_concept_id, $11 education_concept_id, $12 ethnicity_concept_id,
-- $13 phone, $14 email, $15 address, $16 family_id,
-- $17 region_village_concept_id, $18 citizenship_concept_id,
-- $19 identity_type_concept_id, $20 job_concept_id,
-- $21 marital_status_concept_id, $22 blood_type_concept_id,
-- $23 nik_search_hash, $24 updated_by.
UPDATE core.person
SET nik = $2, medical_record_no = $3, full_name = $4, birth_date = $5, birth_place = $6, gender = $7,
    blood_type = $8, marital_status = $9, religion_concept_id = $10, education_concept_id = $11,
    ethnicity_concept_id = $12, phone = $13, email = $14, address = $15, family_id = $16,
    region_village_concept_id = $17, citizenship_concept_id = $18, identity_type_concept_id = $19,
    job_concept_id = $20, marital_status_concept_id = $21, blood_type_concept_id = $22,
    nik_search_hash = $23, updated_by = $24
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeletePerson :exec
UPDATE core.person
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreatePatientAllergy :one
INSERT INTO core.patient_allergy (company_id, person_id, allergy_type, substance_name, reaction, severity, recorded_by, effect_side, event_date)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9)
RETURNING *;

-- name: GetPatientAllergyByID :one
SELECT * FROM core.patient_allergy WHERE id = $1;

-- name: ListPatientAllergiesByPerson :many
SELECT * FROM core.patient_allergy
WHERE person_id = $1 AND is_active
ORDER BY recorded_at DESC;

-- name: UpdatePatientAllergy :one
UPDATE core.patient_allergy
SET reaction = $2, severity = $3, updated_by = $4, effect_side = $5, event_date = $6
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

-- name: UpdatePersonFullName :exec
UPDATE core.person SET full_name = $2, updated_by = $3 WHERE id = $1 AND deleted_at IS NULL;
