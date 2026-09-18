-- internal/db/queries/admission.sql
-- operations.admission, core columns only — see
-- docs/design/specs/2026-09-16-v1-operations-antrian-jadwal-design.md §2/§3.

-- name: CreateAdmission :one
-- Param order: $1 company_id, $2 merchant_id, $3 visit_no, $4 person_id,
-- $5 admission_type, $6 department_id, $7 physician_id, $8 primary_payer_id,
-- $9 complaint, $10 referral_source, $11 note, $12 created_by/updated_by.
INSERT INTO operations.admission (
    company_id, merchant_id, visit_no, person_id, admission_type, department_id,
    physician_id, primary_payer_id, complaint, referral_source, note, created_by, updated_by
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12)
RETURNING *;

-- name: CreateAdmissionGuarantor :one
INSERT INTO operations.admission_guarantor (admission_id, payer_id, policy_number, guarantor_name, sequence, created_by)
VALUES ($1, $2, $3, $4, 1, $5)
RETURNING *;

-- name: GetAdmissionByID :one
SELECT * FROM operations.admission WHERE id = $1 AND deleted_at IS NULL;

-- name: ListAdmissionsForWork :many
SELECT * FROM operations.admission
WHERE merchant_id = $1 AND department_id = $2 AND status = $3 AND deleted_at IS NULL
ORDER BY admission_at
LIMIT $4 OFFSET $5;

-- name: CountTodayAdmissionsByDepartment :one
SELECT count(*) FROM operations.admission
WHERE merchant_id = $1 AND department_id = $2 AND admission_at::date = current_date;

-- name: UpdateAdmission :one
UPDATE operations.admission
SET physician_id = $2, status = $3, discharge_at = $4, updated_by = $5
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;
