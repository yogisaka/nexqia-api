-- internal/db/queries/person_merge.sql
-- Spec 2026-10-07-merge-medical-records §6–§8. Merge field values travel as
-- text so one query serves every column type.

-- name: LockPersonsForMerge :many
SELECT id, row_version, deleted_at IS NULL AS is_live, medical_record_no
FROM core.person WHERE id = ANY(sqlc.arg('ids')::uuid[])
ORDER BY id FOR UPDATE;

-- name: GetPersonMergeFields :one
SELECT id, company_id, row_version, created_at, updated_at,
       COALESCE(medical_record_no, '')::text AS medical_record_no,
       COALESCE(full_name, '')::text AS full_name,
       COALESCE(birth_date::text, '')::text AS birth_date,
       COALESCE(birth_place, '')::text AS birth_place,
       COALESCE(gender, '')::text AS gender,
       COALESCE(blood_type, '')::text AS blood_type,
       COALESCE(marital_status, '')::text AS marital_status,
       COALESCE(religion_concept_id::text, '')::text AS religion_concept_id,
       COALESCE(education_concept_id::text, '')::text AS education_concept_id,
       COALESCE(ethnicity_concept_id::text, '')::text AS ethnicity_concept_id,
       COALESCE(phone, '')::text AS phone,
       COALESCE(email, '')::text AS email,
       COALESCE(address, '')::text AS address,
       COALESCE(family_id::text, '')::text AS family_id,
       COALESCE(photo_url, '')::text AS photo_url,
       COALESCE(region_village_concept_id::text, '')::text AS region_village_concept_id,
       COALESCE(citizenship_concept_id::text, '')::text AS citizenship_concept_id,
       COALESCE(job_concept_id::text, '')::text AS job_concept_id,
       COALESCE(marital_status_concept_id::text, '')::text AS marital_status_concept_id,
       COALESCE(blood_type_concept_id::text, '')::text AS blood_type_concept_id,
       COALESCE(nik, '')::text AS nik,
       COALESCE(nik_search_hash, '')::text AS nik_search_hash,
       COALESCE(identity_type_concept_id::text, '')::text AS identity_type_concept_id
FROM core.person WHERE id = $1 AND deleted_at IS NULL;

-- name: PersonSnapshot :one
SELECT to_jsonb(p)::jsonb AS snapshot FROM core.person p WHERE p.id = $1;

-- name: NameSimilarity :one
SELECT similarity(a.full_name, b.full_name)::float8 AS score
FROM core.person a, core.person b WHERE a.id = $1 AND b.id = $2;

-- name: PersonMergeBlockers :one
SELECT core.person_merge_blockers($1)::jsonb AS blockers;

-- name: PersonMergeCounts :one
SELECT core.person_merge_counts($1)::jsonb AS counts;

-- name: PersonMergeMove :one
SELECT core.person_merge_move(sqlc.arg('survivor_id')::uuid, sqlc.arg('merged_id')::uuid)::jsonb AS moved;

-- name: PersonUnmergeMove :one
SELECT core.person_unmerge_move($1)::jsonb AS result;

-- name: SoftDeleteMergedPerson :exec
UPDATE core.person SET deleted_at = now(), deleted_by = sqlc.arg('actor'), merged_into_person_id = sqlc.arg('survivor_id')
WHERE id = sqlc.arg('merged_id') AND deleted_at IS NULL;

-- name: ApplyMergeFields :exec
UPDATE core.person SET
    full_name = sqlc.arg('full_name'),
    birth_date = NULLIF(sqlc.arg('birth_date')::text, '')::date,
    birth_place = NULLIF(sqlc.arg('birth_place')::text, ''),
    gender = NULLIF(sqlc.arg('gender')::text, ''),
    blood_type = NULLIF(sqlc.arg('blood_type')::text, ''),
    marital_status = NULLIF(sqlc.arg('marital_status')::text, ''),
    religion_concept_id = NULLIF(sqlc.arg('religion_concept_id')::text, '')::uuid,
    education_concept_id = NULLIF(sqlc.arg('education_concept_id')::text, '')::uuid,
    ethnicity_concept_id = NULLIF(sqlc.arg('ethnicity_concept_id')::text, '')::uuid,
    phone = NULLIF(sqlc.arg('phone')::text, ''),
    email = NULLIF(sqlc.arg('email')::text, ''),
    address = NULLIF(sqlc.arg('address')::text, ''),
    family_id = NULLIF(sqlc.arg('family_id')::text, '')::uuid,
    photo_url = NULLIF(sqlc.arg('photo_url')::text, ''),
    region_village_concept_id = NULLIF(sqlc.arg('region_village_concept_id')::text, '')::uuid,
    citizenship_concept_id = NULLIF(sqlc.arg('citizenship_concept_id')::text, '')::uuid,
    job_concept_id = NULLIF(sqlc.arg('job_concept_id')::text, '')::uuid,
    marital_status_concept_id = NULLIF(sqlc.arg('marital_status_concept_id')::text, '')::uuid,
    blood_type_concept_id = NULLIF(sqlc.arg('blood_type_concept_id')::text, '')::uuid,
    nik = NULLIF(sqlc.arg('nik')::text, ''),
    nik_search_hash = NULLIF(sqlc.arg('nik_search_hash')::text, ''),
    identity_type_concept_id = NULLIF(sqlc.arg('identity_type_concept_id')::text, '')::uuid,
    updated_by = sqlc.arg('actor')
WHERE id = sqlc.arg('id');

-- name: RestoreMergedPerson :exec
UPDATE core.person SET deleted_at = NULL, deleted_by = NULL, merged_into_person_id = NULL, updated_by = sqlc.arg('actor')
WHERE id = sqlc.arg('id') AND merged_into_person_id IS NOT NULL;

-- name: InsertMergeLog :one
INSERT INTO core.person_merge_log (surviving_person_id, merged_person_id, reason, merged_by, action,
    identity_check, verified_documents, field_choices, snapshot, moved)
VALUES ($1, $2, $3, $4, 'merge', $5, $6, $7, $8, $9)
RETURNING id, merged_at;

-- name: InsertUnmergeLog :one
INSERT INTO core.person_merge_log (surviving_person_id, merged_person_id, reason, merged_by, action, merge_log_id, moved)
VALUES ($1, $2, $3, $4, 'unmerge', $5, $6)
RETURNING id, merged_at;

-- name: GetMergeLogForUpdate :one
SELECT * FROM core.person_merge_log WHERE id = $1 FOR UPDATE;

-- name: MarkMergeLogReverted :exec
UPDATE core.person_merge_log SET reverted_at = now(), reverted_by = $2 WHERE id = $1;

-- name: CountLaterOpenMerges :one
SELECT count(*) FROM core.person_merge_log
WHERE action = 'merge' AND reverted_at IS NULL AND id <> sqlc.arg('log_id')
  AND merged_at > sqlc.arg('merged_at')
  AND (surviving_person_id = ANY(sqlc.arg('ids')::uuid[]) OR merged_person_id = ANY(sqlc.arg('ids')::uuid[]));

-- name: CountAdmissionsSince :one
-- Visits created on the survivor after the merge (stay there after unmerge).
SELECT count(*) FROM operations.admission WHERE person_id = $1 AND created_at > $2;

-- name: ListMergeLogs :many
SELECT l.*, s.medical_record_no AS survivor_mrn, s.full_name AS survivor_name,
       m.medical_record_no AS merged_mrn, m.full_name AS merged_name
FROM core.person_merge_log l
JOIN core.person s ON s.id = l.surviving_person_id
JOIN core.person m ON m.id = l.merged_person_id
ORDER BY l.merged_at DESC
LIMIT $1 OFFSET $2;

-- name: MarkDuplicatePairMerged :exec
UPDATE core.person_duplicate_pair SET status = 'merged', updated_by = sqlc.arg('actor')
WHERE person_a_id = LEAST(sqlc.arg('a')::uuid, sqlc.arg('b')::uuid) AND person_b_id = GREATEST(sqlc.arg('a')::uuid, sqlc.arg('b')::uuid);
