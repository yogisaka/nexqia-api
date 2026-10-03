-- internal/db/queries/admission.sql
-- operations.admission, core columns only — see
-- 2026-09-16-v1-operations-antrian-jadwal-design.md §2/§3.

-- name: CreateAdmission :one
-- Param order: $1 company_id, $2 merchant_id, $3 visit_no, $4 person_id,
-- $5 admission_type, $6 department_id, $7 physician_id, $8 primary_payer_id,
-- $9 complaint, $10 referral_source, $11 note, $12 created_by/updated_by,
-- $13 schedule_id (optional session pattern, spec 2026-10-01-b §3).
INSERT INTO operations.admission (
    company_id, merchant_id, visit_no, person_id, admission_type, department_id,
    physician_id, primary_payer_id, complaint, referral_source, note, created_by, updated_by,
    schedule_id
)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $12, $13)
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

-- name: SummarizeTodayAdmissionsByPayerType :many
-- "Jenis Pasien" dashboard widget. NULL primary_payer_id (walk-in, belum
-- pilih penjamin) dihitung sebagai 'self_pay' -- default yang sama artinya
-- (bayar sendiri), bukan kategori terpisah.
SELECT COALESCE(p.payer_type, 'self_pay') AS payer_type, count(*) AS total
FROM operations.admission a
LEFT JOIN core.payer p ON p.id = a.primary_payer_id
WHERE a.merchant_id = $1 AND a.admission_at::date = current_date AND a.deleted_at IS NULL
GROUP BY COALESCE(p.payer_type, 'self_pay');

-- name: UpdateAdmission :one
UPDATE operations.admission
SET physician_id = $2, status = $3, discharge_at = $4, updated_by = $5
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: GetPhysicianScheduleForUpdate :one
-- Row-level lock on the schedule pattern so concurrent registrations cannot
-- both pass the quota check (spec 2026-10-01-b §4).
SELECT * FROM operations.physician_schedule WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE;

-- name: CountScheduleAdmissionsForQuota :one
-- Registered (non-cancelled) outpatient counts per payer pool for one
-- schedule pattern on one merchant-local date.
SELECT count(*) FILTER (WHERE pay.payer_type = 'bpjs') AS registered_jkn,
       count(*) FILTER (WHERE COALESCE(pay.payer_type, '') <> 'bpjs') AS registered_other
FROM operations.admission a
LEFT JOIN core.payer pay ON pay.id = a.primary_payer_id
WHERE a.schedule_id = $1 AND a.admission_type = 'outpatient' AND a.status <> 'cancelled'
  AND (a.admission_at AT TIME ZONE $2::text)::date = $3::date;

-- name: GetPayerType :one
SELECT payer_type FROM core.payer WHERE id = $1;
