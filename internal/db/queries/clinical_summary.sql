-- internal/db/queries/clinical_summary.sql
-- Person-scoped clinical read models (plan
-- 2026-11-04-cppt-2a-soap-editor-a-backend Task 4, spec
-- 2026-11-04-cppt-2a-soap-editor-design §7): "Salin dari catatan lalu"
-- person-forms and the patient summary drawer (#32). Small per-domain
-- queries (K2) — composition stays in Go; display joins follow the
-- ListOutpatientAdmissionsByDay pattern (clinical_timeline.sql:32-55).

-- name: ListClinicalFormInstancesByPerson :many
-- Signed, non-deleted form entries of one person across their admissions
-- ("Salin dari catatan lalu", spec §7). Default filter = NON-GIZI only
-- (pola clinicalIsGiziForm, clinical_form.go:131-133); an explicit form_type
-- narrows further, NULL form_type = all non-gizi.
SELECT f.id, f.company_id, f.merchant_id, f.admission_id, f.form_type,
       f.template_id, f.template_version, f.form_data, f.status, f.filled_by,
       f.filled_at, f.signed_by, f.signed_at, f.signature_hash,
       f.counter_signed_by, f.counter_signed_at, f.addendum_to_id,
       f.addendum_reason, f.created_at, f.updated_at, f.deleted_at,
       f.deleted_by, f.row_version
FROM clinical_form.form_instance f
JOIN operations.admission a ON a.id = f.admission_id
LEFT JOIN core.template_clinical_form tc ON tc.template_id = f.template_id
WHERE a.person_id = sqlc.arg('person_id')
  AND a.merchant_id = sqlc.arg('merchant_id')
  AND a.deleted_at IS NULL
  AND f.status = 'signed'
  AND f.deleted_at IS NULL
  AND tc.category IS DISTINCT FROM 'gizi'
  AND f.form_type <> 'ases_gizi_skrining'
  AND (sqlc.narg('form_type')::text IS NULL OR f.form_type = sqlc.narg('form_type')::text)
ORDER BY f.filled_at DESC, f.id
LIMIT sqlc.arg('limit');

-- name: ListPersonRecentVisits :many
-- Latest non-cancelled outpatient visits of one person (drawer #32; fase 2a
-- is rawat jalan only — konsisten clinical_timeline.sql:50, ranap menyusul).
SELECT a.id, a.visit_no, a.admission_at, a.status AS admission_status,
       COALESCE(d.name, '') AS department_name,
       dok.full_name AS physician_name
FROM operations.admission a
LEFT JOIN core.department d ON d.id = a.department_id
LEFT JOIN core.physician ph ON ph.id = a.physician_id
LEFT JOIN core.person dok ON dok.id = ph.person_id
WHERE a.person_id = sqlc.arg('person_id')
  AND a.merchant_id = sqlc.arg('merchant_id')
  AND a.deleted_at IS NULL
  AND a.admission_type = 'outpatient'
  AND a.status <> 'cancelled'
ORDER BY a.admission_at DESC, a.id
LIMIT sqlc.arg('limit');

-- name: ListPersonDiagnoses :many
-- Diagnoses of one person across their non-cancelled admissions
-- (active_diagnoses source; dedup + newest-first selection in Go).
SELECT d.id, d.admission_id, d.dx_role, d.primary_system, d.primary_code,
       d.primary_display, d.icd10_code, d.created_at
FROM clinical.diagnosis d
JOIN operations.admission a ON a.id = d.admission_id
WHERE a.person_id = sqlc.arg('person_id')
  AND a.merchant_id = sqlc.arg('merchant_id')
  AND a.deleted_at IS NULL
  AND a.status <> 'cancelled'
ORDER BY d.created_at DESC, d.id
LIMIT sqlc.arg('limit');

-- name: GetPersonLatestSignedSoap :one
-- Latest signed soap_dokter entry across the person's admissions
-- (latest_recommendation source, spec §7).
SELECT f.id, f.admission_id, f.form_data, f.signed_at
FROM clinical_form.form_instance f
JOIN operations.admission a ON a.id = f.admission_id
WHERE a.person_id = sqlc.arg('person_id')
  AND a.merchant_id = sqlc.arg('merchant_id')
  AND a.deleted_at IS NULL
  AND f.form_type = 'soap_dokter'
  AND f.status = 'signed'
  AND f.deleted_at IS NULL
ORDER BY f.signed_at DESC, f.id
LIMIT 1;

-- name: PersonHasMerchantAdmission :one
-- Existence check for the person-forms 404 gate: person must have at least
-- one admission in the caller's merchant (spec §7: person tidak ada / beda
-- merchant → 404).
SELECT EXISTS (
  SELECT 1 FROM operations.admission
  WHERE person_id = sqlc.arg('person_id')
    AND merchant_id = sqlc.arg('merchant_id')
    AND deleted_at IS NULL
);
