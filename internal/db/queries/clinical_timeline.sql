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

-- name: ListOutpatientAdmissionsByDay :many
-- Daily completeness board (plan 2026-10-21-cppt-rajal-b Task 3, spec §11):
-- outpatient visits of one merchant inside [from, to) — the handler passes
-- the merchant-local day [localDayStart, +24h). Composition of the per-check
-- status stays in Go; this query only joins the display names.
SELECT a.id, a.visit_no, per.full_name AS person_name,
       COALESCE(d.name, '') AS department_name
FROM operations.admission a
JOIN core.person per ON per.id = a.person_id
LEFT JOIN core.department d ON d.id = a.department_id
WHERE a.merchant_id = $1
  AND a.admission_type = 'outpatient'
  AND a.deleted_at IS NULL
  AND a.admission_at >= $2::timestamptz
  AND a.admission_at < $3::timestamptz
ORDER BY a.admission_at, a.id;
