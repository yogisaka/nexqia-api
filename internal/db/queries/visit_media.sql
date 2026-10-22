-- internal/db/queries/visit_media.sql
-- operations.visit_token, operations.visit_media_config, person card and
-- admission visit context — see 2026-10-20-qr-visit-media-design.md §2/§4.

-- name: CreateVisitToken :one
INSERT INTO operations.visit_token (company_id, merchant_id, admission_id, token_hash, media, expires_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7) RETURNING *;

-- name: GetVisitTokenByHash :one
SELECT * FROM operations.visit_token WHERE token_hash = $1;

-- name: GetActiveVisitTokenByAdmission :one
SELECT * FROM operations.visit_token WHERE admission_id = $1 AND closed_at IS NULL;

-- name: CloseActiveVisitTokensByAdmission :execrows
UPDATE operations.visit_token
SET closed_at = now(), closed_by = $2, close_reason = $3, updated_by = $2
WHERE admission_id = $1 AND closed_at IS NULL;

-- name: ListVisitMediaConfig :many
SELECT * FROM operations.visit_media_config WHERE merchant_id = $1 ORDER BY department_id NULLS FIRST;

-- name: UpsertVisitMediaConfig :one
INSERT INTO operations.visit_media_config (company_id, merchant_id, department_id, media, created_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (merchant_id, COALESCE(department_id, '00000000-0000-0000-0000-000000000000'::uuid))
DO UPDATE SET media = EXCLUDED.media, updated_by = EXCLUDED.created_by
RETURNING *;

-- name: DeleteVisitMediaOverride :execrows
DELETE FROM operations.visit_media_config WHERE merchant_id = $1 AND department_id = $2;

-- name: GetDepartmentMerchant :one
SELECT id, merchant_id FROM core.department WHERE id = $1;

-- name: SetPersonCard :exec
UPDATE core.person SET card_token_hash = $2, card_issued_at = now(), card_issued_by = $3 WHERE id = $1;

-- name: GetPersonByCardToken :one
SELECT id, company_id, merged_into_person_id, full_name, birth_date, medical_record_no, gender
FROM core.person WHERE card_token_hash = $1;

-- name: CountActivePatientAllergies :one
SELECT count(*) FROM core.patient_allergy WHERE person_id = $1 AND is_active;

-- name: GetAdmissionVisitContext :one
SELECT a.id AS admission_id, a.merchant_id, a.fall_risk, a.person_id,
       p.full_name, p.birth_date, p.medical_record_no
FROM operations.admission a
JOIN core.person p ON p.id = a.person_id
WHERE a.id = $1;
