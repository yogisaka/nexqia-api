-- internal/db/queries/person_duplicate.sql
-- Spec 2026-10-07-merge-medical-records §8.4. Blocking by birth date +
-- gender keeps the self-join small; the NIK rule needs no blocking.

-- name: ListDuplicateCandidates :many
WITH live AS (
    SELECT p.id, p.medical_record_no, p.full_name, p.birth_date, p.gender, p.phone, p.address, p.nik_search_hash, p.created_at
    FROM core.person p WHERE p.company_id = sqlc.arg('company_id')::uuid AND p.deleted_at IS NULL
), pairs AS (
    SELECT a.id AS a_id, b.id AS b_id,
        CASE
            WHEN a.nik_search_hash IS NOT NULL AND a.nik_search_hash = b.nik_search_hash THEN 100
            WHEN a.birth_date = b.birth_date AND a.gender = b.gender AND similarity(a.full_name, b.full_name) >= 0.5 THEN 80
            WHEN a.phone IS NOT NULL AND a.phone <> '' AND a.phone = b.phone AND similarity(a.full_name, b.full_name) >= 0.5 THEN 70
            WHEN a.birth_date = b.birth_date AND a.address IS NOT NULL AND b.address IS NOT NULL AND similarity(a.address, b.address) >= 0.5 THEN 40
        END AS score,
        CASE
            WHEN a.nik_search_hash IS NOT NULL AND a.nik_search_hash = b.nik_search_hash THEN 'same_nik'
            WHEN a.birth_date = b.birth_date AND a.gender = b.gender AND similarity(a.full_name, b.full_name) >= 0.5 THEN 'birth_gender_name'
            WHEN a.phone IS NOT NULL AND a.phone <> '' AND a.phone = b.phone AND similarity(a.full_name, b.full_name) >= 0.5 THEN 'phone_name'
            WHEN a.birth_date = b.birth_date AND a.address IS NOT NULL AND b.address IS NOT NULL AND similarity(a.address, b.address) >= 0.5 THEN 'birth_address'
        END AS reason
    FROM live a JOIN live b ON a.id < b.id
     AND (a.nik_search_hash = b.nik_search_hash OR a.birth_date = b.birth_date OR (a.phone <> '' AND a.phone = b.phone))
)
SELECT p.a_id, p.b_id, p.score, p.reason,
       COALESCE(d.status, '')::text AS status, COALESCE(d.note, '')::text AS note,
       a.medical_record_no AS a_mrn, a.full_name AS a_name, a.birth_date AS a_birth_date, a.gender AS a_gender, a.created_at AS a_created_at,
       b.medical_record_no AS b_mrn, b.full_name AS b_name, b.birth_date AS b_birth_date, b.gender AS b_gender, b.created_at AS b_created_at
FROM pairs p
JOIN live a ON a.id = p.a_id
JOIN live b ON b.id = p.b_id
LEFT JOIN core.person_duplicate_pair d ON d.person_a_id = p.a_id AND d.person_b_id = p.b_id
WHERE p.score IS NOT NULL AND COALESCE(d.status, '') NOT IN ('dismissed', 'merged')
ORDER BY (d.status = 'requested') DESC NULLS LAST, p.score DESC, a.full_name
LIMIT sqlc.arg('row_limit') OFFSET sqlc.arg('row_offset');

-- name: CheckNewPatientDuplicates :many
SELECT id, medical_record_no, full_name, birth_date, gender,
    CASE
        WHEN birth_date = sqlc.narg('birth_date')::date AND gender = sqlc.narg('gender')::text AND similarity(full_name, sqlc.arg('full_name')::text) >= 0.5 THEN 80
        WHEN sqlc.narg('phone')::text <> '' AND phone = sqlc.narg('phone')::text AND similarity(full_name, sqlc.arg('full_name')::text) >= 0.5 THEN 70
    END::int AS score
FROM core.person
WHERE company_id = $1 AND deleted_at IS NULL
  AND ((birth_date = sqlc.narg('birth_date')::date AND gender = sqlc.narg('gender')::text AND similarity(full_name, sqlc.arg('full_name')::text) >= 0.5)
    OR (sqlc.narg('phone')::text <> '' AND phone = sqlc.narg('phone')::text AND similarity(full_name, sqlc.arg('full_name')::text) >= 0.5))
ORDER BY score DESC, full_name
LIMIT 10;

-- name: UpsertDuplicatePair :one
INSERT INTO core.person_duplicate_pair (person_a_id, person_b_id, status, note, created_by, updated_by)
VALUES (LEAST(sqlc.arg('a')::uuid, sqlc.arg('b')::uuid), GREATEST(sqlc.arg('a')::uuid, sqlc.arg('b')::uuid),
        sqlc.arg('status'), sqlc.narg('note'), sqlc.arg('actor'), sqlc.arg('actor'))
ON CONFLICT (person_a_id, person_b_id) DO UPDATE SET status = EXCLUDED.status, note = EXCLUDED.note, updated_by = EXCLUDED.updated_by
WHERE core.person_duplicate_pair.status <> 'merged'
RETURNING id, status;
