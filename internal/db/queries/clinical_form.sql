-- internal/db/queries/clinical_form.sql
-- CPPT Fase 1 (spec 2026-10-21-cppt-rajal-fase1-design §4, §6-§8):
-- clinical_form templates/instances, visit diagnoses, nutrition orders.

-- name: ListClinicalFormTemplates :many
SELECT f.id, t.id AS template_id, f.form_type, f.category, f.sections,
       t.code, t.name, t.scope, t.version
FROM core.template_clinical_form f
JOIN core.template t ON t.id = f.template_id
WHERE t.kind = 'clinical_form' AND t.is_active
  AND (t.company_id IS NULL OR t.company_id = $1)
  AND (t.merchant_id IS NULL OR t.merchant_id = $2)
ORDER BY t.code, f.form_type;

-- name: CreateClinicalFormInstance :one
INSERT INTO clinical_form.form_instance
    (company_id, merchant_id, admission_id, form_type, template_id, template_version,
     form_data, filled_by, addendum_to_id, addendum_reason)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING *;

-- name: GetClinicalFormInstance :one
SELECT id, company_id, merchant_id, admission_id, form_type, template_id,
       template_version, form_data, status, filled_by, filled_at, signed_by,
       signed_at, signature_hash, counter_signed_by, counter_signed_at,
       addendum_to_id, addendum_reason, created_at, updated_at, deleted_at,
       deleted_by, row_version
FROM clinical_form.form_instance
WHERE id = $1 AND deleted_at IS NULL;

-- name: GetClinicalFormInstanceForUpdate :one
SELECT id, company_id, merchant_id, admission_id, form_type, template_id,
       template_version, form_data, status, filled_by, filled_at, signed_by,
       signed_at, signature_hash, counter_signed_by, counter_signed_at,
       addendum_to_id, addendum_reason, created_at, updated_at, deleted_at,
       deleted_by, row_version
FROM clinical_form.form_instance
WHERE id = $1 AND deleted_at IS NULL
FOR UPDATE;

-- name: ListClinicalFormInstancesByAdmission :many
SELECT id, company_id, merchant_id, admission_id, form_type, template_id,
       template_version, form_data, status, filled_by, filled_at, signed_by,
       signed_at, signature_hash, counter_signed_by, counter_signed_at,
       addendum_to_id, addendum_reason, created_at, updated_at, deleted_at,
       deleted_by, row_version
FROM clinical_form.form_instance
WHERE admission_id = $1 AND ($2 OR deleted_at IS NULL)
ORDER BY filled_at, id;

-- name: UpdateClinicalFormInstanceDraft :one
UPDATE clinical_form.form_instance
SET form_data = $2
WHERE id = $1 AND status = 'draft' AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteClinicalFormInstance :one
UPDATE clinical_form.form_instance
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND status = 'draft' AND deleted_at IS NULL
RETURNING id;

-- name: SignClinicalFormInstance :one
UPDATE clinical_form.form_instance
SET status = 'signed', signed_by = $2, signed_at = $3, signature_hash = $4
WHERE id = $1 AND status = 'draft' AND deleted_at IS NULL
RETURNING *;

-- name: CountersignClinicalFormInstance :one
UPDATE clinical_form.form_instance
SET counter_signed_by = $2, counter_signed_at = $3
WHERE id = $1 AND status = 'signed' AND counter_signed_at IS NULL AND deleted_at IS NULL
RETURNING *;

-- name: ListClinicalFormAddendums :many
SELECT id, company_id, merchant_id, admission_id, form_type, template_id,
       template_version, form_data, status, filled_by, filled_at, signed_by,
       signed_at, signature_hash, counter_signed_by, counter_signed_at,
       addendum_to_id, addendum_reason, created_at, updated_at, deleted_at,
       deleted_by, row_version
FROM clinical_form.form_instance
WHERE addendum_to_id = $1 AND deleted_at IS NULL
ORDER BY filled_at, id;

-- name: CreateVisitDiagnosis :one
INSERT INTO clinical.diagnosis
    (company_id, merchant_id, admission_id, form_instance_id, dx_role, concept_id,
     primary_system, primary_code, primary_display,
     icd10_concept_id, icd10_code, icd10_display, icd10_source, icd10_map_set,
     icd10_chosen_by, icd10_chosen_at, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $15, $16, $17)
RETURNING *;

-- name: ListAdmissionDiagnoses :many
SELECT id, company_id, merchant_id, admission_id, form_instance_id, dx_role,
       concept_id, primary_system, primary_code, primary_display,
       icd10_concept_id, icd10_code, icd10_display, icd10_source, icd10_map_set,
       icd10_chosen_by, icd10_chosen_at, created_at, created_by, row_version
FROM clinical.diagnosis
WHERE admission_id = $1
ORDER BY created_at, id;

-- name: CreateNutritionOrder :one
INSERT INTO clinical.nutrition_order
    (company_id, merchant_id, admission_id, ordering_physician_id, ordered_by,
     diet_type_id, texture, route, calorie_target, restriction_note, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11)
RETURNING *;

-- name: ListNutritionOrdersByAdmission :many
SELECT id, company_id, merchant_id, admission_id, ordering_physician_id,
       ordered_by, diet_type_id, texture, route, calorie_target, restriction_note,
       status, started_at, ended_at, created_at, created_by, updated_at,
       updated_by, row_version
FROM clinical.nutrition_order
WHERE admission_id = $1
ORDER BY started_at DESC, id;

-- name: GetNutritionOrder :one
SELECT id, company_id, merchant_id, admission_id, ordering_physician_id,
       ordered_by, diet_type_id, texture, route, calorie_target, restriction_note,
       status, started_at, ended_at, created_at, created_by, updated_at,
       updated_by, row_version
FROM clinical.nutrition_order
WHERE id = $1;

-- name: UpdateNutritionOrder :one
UPDATE clinical.nutrition_order
SET ordering_physician_id = $2, diet_type_id = $3, texture = $4, route = $5,
    calorie_target = $6, restriction_note = $7, status = $8, ended_at = $9,
    updated_by = $10, row_version = row_version + 1
WHERE id = $1 AND row_version = $11
RETURNING *;
