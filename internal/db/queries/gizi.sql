-- internal/db/queries/gizi.sql
-- Gizi (nutrition care) endpoints — plan 2026-11-13-gizi-nutrition-care
-- chunk 1 Task 3 (spec 2026-11-13-gizi-nutrition-care-spec §4.1, §5).
-- Tabel: clinical.anthropometry / nutrition_episode / nutrition_profile_change /
-- nutrition_screening_result + ref.growth_* (migration 000097_gizi_fondasi).

-- Admission + person context: handler isi gender/age_days antropometri
-- (zona waktu merchant, pola merchant_day.go) dan cek kepemilikan tenant.
-- name: GetAdmissionForGizi :one
SELECT a.id, a.company_id, a.merchant_id, a.person_id, a.admission_type, a.status,
       p.gender, p.birth_date
FROM operations.admission a
JOIN core.person p ON p.id = a.person_id
WHERE a.id = $1;

-- name: InsertAnthropometry :one
INSERT INTO clinical.anthropometry
    (company_id, merchant_id, person_id, admission_id, measured_at, gender,
     age_days, bb_kg, tb_cm, source_ref, extras, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING id, company_id, merchant_id, person_id, admission_id, measured_at,
          gender, age_days, bb_kg, tb_cm, source_ref, extras,
          zscore_bbu, zscore_tbu, zscore_bbtb, zscore_imtu, zscore_lku, zscore_lila,
          imt, imt_kategori, status, curve_version, created_at, created_by, row_version;

-- name: ListAnthropometryByAdmission :many
SELECT a.id, a.person_id, a.admission_id, a.measured_at, a.gender, a.age_days,
       a.bb_kg, a.tb_cm, a.source_ref, a.extras,
       a.zscore_bbu, a.zscore_tbu, a.zscore_bbtb, a.zscore_imtu, a.zscore_lku,
       a.zscore_lila, a.imt, a.imt_kategori, a.status, a.curve_version,
       a.row_version, p.full_name AS person_name
FROM clinical.anthropometry a
JOIN core.person p ON p.id = a.person_id
WHERE a.admission_id = $1
ORDER BY a.measured_at DESC;

-- name: GetLatestAnthropometryByPerson :one
SELECT id, company_id, merchant_id, person_id, admission_id, measured_at,
       gender, age_days, bb_kg, tb_cm, source_ref, extras,
       zscore_bbu, zscore_tbu, zscore_bbtb, zscore_imtu, zscore_lku, zscore_lila,
       imt, imt_kategori, status, curve_version, created_at, created_by, row_version
FROM clinical.anthropometry
WHERE person_id = $1
ORDER BY measured_at DESC
LIMIT 1;

-- name: CreateNutritionEpisode :one
INSERT INTO clinical.nutrition_episode
    (company_id, merchant_id, person_id, admission_id, profile, risk_override,
     extras, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING id, company_id, merchant_id, person_id, admission_id, profile,
          risk_override, status, extras, started_at, closed_at, created_at,
          created_by, updated_at, updated_by, row_version;

-- name: GetActiveNutritionEpisodeByAdmission :one
SELECT id, company_id, merchant_id, person_id, admission_id, profile,
       risk_override, status, extras, started_at, closed_at, created_at,
       created_by, updated_at, updated_by, row_version
FROM clinical.nutrition_episode
WHERE admission_id = $1 AND status = 'active';

-- name: GetNutritionEpisodeByID :one
SELECT id, company_id, merchant_id, person_id, admission_id, profile,
       risk_override, status, extras, started_at, closed_at, created_at,
       created_by, updated_at, updated_by, row_version
FROM clinical.nutrition_episode
WHERE id = $1;

-- Optimistic lock: 0 baris (ErrNoRows) → handler 409 "row_version konflik".
-- name: UpdateNutritionEpisode :one
UPDATE clinical.nutrition_episode
SET profile = $2, extras = $3, risk_override = $4, status = $5,
    closed_at = $6, updated_by = $7, row_version = row_version + 1
WHERE id = $1 AND row_version = $8
RETURNING id, company_id, merchant_id, person_id, admission_id, profile,
          risk_override, status, extras, started_at, closed_at, created_at,
          created_by, updated_at, updated_by, row_version;

-- Guard jalur 1 (FR-GZ-01): tutup episode aktif admission ini. 0 baris sah
-- (belum ada episode aktif); UNIQUE partial index menjamin 1 aktif/admission.
-- name: CloseActiveNutritionEpisodeByAdmission :one
UPDATE clinical.nutrition_episode
SET status = 'closed', closed_at = now(), updated_by = $2, row_version = row_version + 1
WHERE admission_id = $1 AND status = 'active'
RETURNING id;

-- name: InsertNutritionProfileChange :one
INSERT INTO clinical.nutrition_profile_change
    (company_id, merchant_id, person_id, episode_id, from_profile, to_profile, changed_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, company_id, merchant_id, person_id, episode_id, from_profile,
          to_profile, changed_at, changed_by, reverted_at, reverted_by,
          created_at, row_version;

-- name: GetLastUnrevertedProfileChange :one
SELECT id, company_id, merchant_id, person_id, episode_id, from_profile,
       to_profile, changed_at, changed_by, reverted_at, reverted_by,
       created_at, row_version
FROM clinical.nutrition_profile_change
WHERE episode_id = $1 AND reverted_at IS NULL
ORDER BY changed_at DESC
LIMIT 1;

-- name: MarkProfileChangeReverted :one
UPDATE clinical.nutrition_profile_change
SET reverted_at = $2, reverted_by = $3, row_version = row_version + 1
WHERE id = $1
RETURNING id;

-- name: ListProfileChangesByEpisode :many
SELECT c.from_profile, c.to_profile, c.changed_at, c.reverted_at,
       u.username AS changed_by_name
FROM clinical.nutrition_profile_change c
LEFT JOIN core.app_user u ON u.id = c.changed_by
WHERE c.episode_id = $1
ORDER BY c.changed_at DESC;

-- name: ListNutritionEpisodesByPerson :many
SELECT id, company_id, merchant_id, person_id, admission_id, profile,
       risk_override, status, extras, started_at, closed_at, created_at,
       created_by, updated_at, updated_by, row_version
FROM clinical.nutrition_episode
WHERE person_id = $1
ORDER BY started_at DESC;

-- Jumlah kunjungan (admission) person untuk badge riwayat episode.
-- name: CountKunjunganByPerson :one
SELECT count(*) FROM operations.admission WHERE person_id = $1;

-- Hasil skrining terbaru PER instrument untuk admission (badge + Tinjau).
-- name: ListLatestNutritionScreeningByAdmission :many
SELECT DISTINCT ON (instrument)
       id, instrument, score, max_score, interpretation, bands, payload, calculated_at
FROM clinical.nutrition_screening_result
WHERE admission_id = $1
ORDER BY instrument, calculated_at DESC;

-- Hook FR-GZ-02 (plan chunk 1 Task 4, spec §5 teknis C): idempoten per
-- (form_instance_id, instrument) — UPDATE dulu; 0 baris → INSERT (persist).
-- row_version naik via trg_touch_row (explicit +1 mengikuti pola UpdateNutritionEpisode).
-- name: UpdateGiziScreeningResultByForm :execrows
UPDATE clinical.nutrition_screening_result
SET score = $2, interpretation = $3, bands = $4, payload = $5,
    episode_id = $6, calculated_at = now(), calculated_by = $7,
    row_version = row_version + 1
WHERE form_instance_id = $1 AND instrument = 'mst';

-- name: InsertGiziScreeningResult :one
INSERT INTO clinical.nutrition_screening_result
    (company_id, merchant_id, person_id, admission_id, episode_id,
     form_instance_id, instrument, score, max_score, interpretation,
     bands, payload, calculated_by, created_by)
VALUES ($1, $2, $3, $4, $5, $6, 'mst', $7, 5, $8, $9, $10, $11, $12)
RETURNING id;

-- Pra-000098 template asuhan_gizi belum ada → count 0 sah.
-- name: CountAsuhanGiziByAdmission :one
SELECT count(*) FROM clinical_form.form_instance
WHERE admission_id = $1 AND form_type = 'asuhan_gizi';
