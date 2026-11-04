-- internal/db/queries/clinical_timeline.sql
-- Composed clinical timeline read model per admission (plan
-- 2026-10-21-cppt-rajal-b Task 2, spec 2026-10-21-cppt-rajal-fase1-design §9).
-- The timeline handler composes EXISTING per-domain queries — no giant SQL
-- join (spec §9 / docs/10:225):
--   operations.admission      → GetAdmissionByID                (admission.sql:23)
--   core.person               → GetPersonByID                   (person.sql:29)
--   core.patient_allergy      → ListPatientAllergiesByPerson    (person.sql:85, is_active only)
--   clinical_form.form_instance → ListClinicalFormInstancesByAdmission (clinical_form.sql:41)
--   clinical.diagnosis        → ListAdmissionDiagnoses          (clinical_form.sql:94)
--   clinical.nutrition_order  → ListNutritionOrdersByAdmissionWithDiet (clinical_form.sql:165,
--                               diet_type join already existed from plan B Task 1)
-- The ordering merge (newest first) is done in Go (internal/server/
-- clinical_timeline.go), not in SQL.

-- name: GetUserDisplayName :one
-- Fase 2a clinical preview (plan 2026-11-04-clinical-board-fase2a-plan Task 2):
-- display name of one active user — person full name, else username.
SELECT COALESCE(pe.full_name, u.username) AS display_name
FROM core.app_user u
LEFT JOIN core.person pe ON pe.id = u.person_id
WHERE u.id = $1 AND u.deleted_at IS NULL;

-- name: ListOutpatientAdmissionsByDay :many
-- Daily completeness board (plan 2026-10-21-cppt-rajal-b Task 3, spec §11):
-- outpatient visits of one merchant inside [from, to) — the handler passes
-- the merchant-local day [localDayStart, +24h). Composition of the per-check
-- status stays in Go; this query only joins the display names. Fase 2a adds
-- the visit metadata columns (mrn/gender/birth_date/physician/payer/status)
-- and excludes cancelled admissions (board noise); physician_name/payer_name
-- stay WITHOUT COALESCE so a NULL id maps to JSON null naturally.
SELECT a.id, a.visit_no, a.person_id,
       per.full_name AS person_name,
       per.medical_record_no AS mrn,
       per.gender,
       per.birth_date,
       a.department_id,
       COALESCE(d.name, '') AS department_name,
       dok.full_name AS physician_name,
       a.admission_at,
       a.status AS admission_status,
       py.name AS payer_name
FROM operations.admission a
JOIN core.person per ON per.id = a.person_id
LEFT JOIN core.department d ON d.id = a.department_id
LEFT JOIN core.physician ph ON ph.id = a.physician_id
LEFT JOIN core.person dok ON dok.id = ph.person_id
LEFT JOIN core.payer py ON py.id = a.primary_payer_id
WHERE a.merchant_id = $1
  AND a.admission_type = 'outpatient'
  AND a.deleted_at IS NULL
  AND a.status <> 'cancelled'
  AND a.admission_at >= $2::timestamptz
  AND a.admission_at < $3::timestamptz
ORDER BY a.admission_at, a.id;

-- name: ListClinicalFormInstancesByAdmissionIDs :many
-- Fase 2a batched read model (anti-N+1): all non-deleted form instances of
-- many admissions in one query; grouping per admission stays in Go.
SELECT id, company_id, merchant_id, admission_id, form_type, template_id,
       template_version, form_data, status, filled_by, filled_at, signed_by,
       signed_at, signature_hash, counter_signed_by, counter_signed_at,
       addendum_to_id, addendum_reason, created_at, updated_at, deleted_at,
       deleted_by, row_version
FROM clinical_form.form_instance
WHERE admission_id = ANY(sqlc.arg('ids')::uuid[]) AND deleted_at IS NULL
ORDER BY admission_id, filled_at, id;

-- name: ListAdmissionDiagnosesByAdmissionIDs :many
-- Fase 2a batched read model: diagnoses of many admissions in one query.
SELECT id, company_id, merchant_id, admission_id, form_instance_id, dx_role,
       concept_id, primary_system, primary_code, primary_display,
       icd10_concept_id, icd10_code, icd10_display, icd10_source, icd10_map_set,
       icd10_chosen_by, icd10_chosen_at, created_at, created_by, row_version
FROM clinical.diagnosis
WHERE admission_id = ANY(sqlc.arg('ids')::uuid[])
ORDER BY admission_id, created_at, id;

-- name: ListLatestQueueTicketsByAdmissionIDs :many
-- Fase 2a batched read model: latest valid ticket per admission; poli beats
-- loket (CASE sort) and cancelled/skipped tickets are noise (docs/08:97-98).
SELECT DISTINCT ON (admission_id) admission_id, queue_type, queue_number, created_at
FROM operations.queue
WHERE admission_id = ANY(sqlc.arg('ids')::uuid[])
  AND status NOT IN ('cancelled','skipped')
ORDER BY admission_id,
         CASE queue_type WHEN 'poli' THEN 0 WHEN 'loket' THEN 1 ELSE 2 END,
         created_at DESC;
