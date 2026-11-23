-- internal/db/queries/kebidanan.sql
-- Kebidanan (ANC & episode kehamilan) — plan 2026-11-22-kebidanan-anc-f1 Task 1.
-- Tabel: operations.care_episode / clinical.midwifery_pregnancy / midwifery_anc_visit /
-- midwifery_pe_screen / midwifery_usg / clinical.person_riwayat_vaksin /
-- ref.immunization_schedule (migration 000109_kebidanan_anc_f1).

-- name: CreateCareEpisode :one
INSERT INTO operations.care_episode
    (company_id, merchant_id, person_id, kind, ordinal_no, status)
VALUES ($1, $2, $3, $4, $5, 'active')
RETURNING id, company_id, merchant_id, person_id, kind, ordinal_no, status,
          started_at, closed_at, close_context, row_version, created_at, updated_at;

-- name: CountCareEpisodePersonKind :one
SELECT count(*) AS total FROM operations.care_episode
WHERE person_id = $1 AND kind = $2;

-- name: GetActiveCareEpisodeByPerson :one
SELECT id, company_id, merchant_id, person_id, kind, ordinal_no, status,
       started_at, closed_at, close_context, row_version, created_at, updated_at
FROM operations.care_episode
WHERE person_id = $1 AND kind = 'kehamilan' AND status = 'active';

-- name: ListCareEpisodesByPerson :many
SELECT id, company_id, merchant_id, person_id, kind, ordinal_no, status,
       started_at, closed_at, close_context, row_version, created_at, updated_at
FROM operations.care_episode
WHERE person_id = $1 AND kind = 'kehamilan'
ORDER BY ordinal_no DESC;

-- Optimistic lock: 0 baris (ErrNoRows) → handler 409 "row_version konflik".
-- name: CloseCareEpisode :one
UPDATE operations.care_episode
SET status = $2, closed_at = $3, close_context = $4, row_version = row_version + 1
WHERE id = $1 AND row_version = $5
RETURNING id, company_id, merchant_id, person_id, kind, ordinal_no, status,
          started_at, closed_at, close_context, row_version, created_at, updated_at;

-- name: CreateMidwiferyPregnancy :one
INSERT INTO clinical.midwifery_pregnancy
    (company_id, merchant_id, care_episode_id, gravida, para, abortus,
     riwayat_persalinan, jarak_kehamilan_th, hpht, uk_source, uk_usg_t1_weeks, hpl)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12)
RETURNING id, company_id, merchant_id, care_episode_id, gravida, para, abortus,
          riwayat_persalinan, jarak_kehamilan_th, hpht, uk_source, uk_usg_t1_weeks,
          hpl, row_version, created_at, updated_at;

-- name: GetMidwiferyPregnancy :one
SELECT id, company_id, merchant_id, care_episode_id, gravida, para, abortus,
       riwayat_persalinan, jarak_kehamilan_th, hpht, uk_source, uk_usg_t1_weeks,
       hpl, row_version, created_at, updated_at
FROM clinical.midwifery_pregnancy
WHERE care_episode_id = $1;

-- Optimistic lock: 0 baris (ErrNoRows) → handler 409 "row_version konflik".
-- name: UpdateMidwiferyPregnancy :one
UPDATE clinical.midwifery_pregnancy
SET gravida = $2, para = $3, abortus = $4, riwayat_persalinan = $5,
    jarak_kehamilan_th = $6, hpht = $7, uk_source = $8, uk_usg_t1_weeks = $9,
    hpl = $10, row_version = row_version + 1
WHERE care_episode_id = $1 AND row_version = $11
RETURNING id, company_id, merchant_id, care_episode_id, gravida, para, abortus,
          riwayat_persalinan, jarak_kehamilan_th, hpht, uk_source, uk_usg_t1_weeks,
          hpl, row_version, created_at, updated_at;

-- name: CreateAncVisit :one
INSERT INTO clinical.midwifery_anc_visit
    (company_id, merchant_id, care_episode_id, admission_id, visited_at,
     tfu_cm, djj, presentation, placenta_location, inspekulo, tanda_bahaya,
     jiwa, imunisasi_lainnya, next_visit_date)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14)
RETURNING id, company_id, merchant_id, care_episode_id, admission_id, visited_at,
          tfu_cm, djj, presentation, placenta_location, inspekulo, tanda_bahaya,
          jiwa, imunisasi_lainnya, next_visit_date, row_version, created_at, updated_at;

-- name: ListAncVisitsByEpisode :many
SELECT id, company_id, merchant_id, care_episode_id, admission_id, visited_at,
       tfu_cm, djj, presentation, placenta_location, inspekulo, tanda_bahaya,
       jiwa, imunisasi_lainnya, next_visit_date, row_version, created_at, updated_at
FROM clinical.midwifery_anc_visit
WHERE care_episode_id = $1
ORDER BY visited_at DESC;

-- name: GetLatestAncVisit :one
SELECT id, company_id, merchant_id, care_episode_id, admission_id, visited_at,
       tfu_cm, djj, presentation, placenta_location, inspekulo, tanda_bahaya,
       jiwa, imunisasi_lainnya, next_visit_date, row_version, created_at, updated_at
FROM clinical.midwifery_anc_visit
WHERE care_episode_id = $1
ORDER BY visited_at DESC
LIMIT 1;

-- name: CreatePeScreen :one
INSERT INTO clinical.midwifery_pe_screen
    (company_id, merchant_id, care_episode_id, screened_at, factors, band, rujukan_flag)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING id, company_id, merchant_id, care_episode_id, screened_at, factors,
          band, rujukan_flag, row_version, created_at, updated_at;

-- name: ListPeScreensByEpisode :many
SELECT id, company_id, merchant_id, care_episode_id, screened_at, factors,
       band, rujukan_flag, row_version, created_at, updated_at
FROM clinical.midwifery_pe_screen
WHERE care_episode_id = $1
ORDER BY screened_at DESC;

-- name: CreateMidwiferyUsg :one
INSERT INTO clinical.midwifery_usg
    (company_id, merchant_id, care_episode_id, trimester, usg_date,
     crl_mm, gs_mm, bpd_mm, hc_mm, ac_mm, fl_mm, efw_g, sdp)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13)
RETURNING id, company_id, merchant_id, care_episode_id, trimester, usg_date,
          crl_mm, gs_mm, bpd_mm, hc_mm, ac_mm, fl_mm, efw_g, sdp,
          row_version, created_at, updated_at;

-- name: ListUsgByEpisode :many
SELECT id, company_id, merchant_id, care_episode_id, trimester, usg_date,
       crl_mm, gs_mm, bpd_mm, hc_mm, ac_mm, fl_mm, efw_g, sdp,
       row_version, created_at, updated_at
FROM clinical.midwifery_usg
WHERE care_episode_id = $1
ORDER BY usg_date DESC;

-- name: CreatePersonVaksin :one
INSERT INTO clinical.person_riwayat_vaksin
    (company_id, merchant_id, person_id, antigen_slug, dosis_ke, given_date,
     no_batch, tempat, tempat_keterangan, pemberi)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10)
RETURNING id, company_id, merchant_id, person_id, antigen_slug, dosis_ke,
          given_date, no_batch, tempat, tempat_keterangan, pemberi,
          row_version, created_at, updated_at;

-- name: ListPersonVaksin :many
SELECT id, company_id, merchant_id, person_id, antigen_slug, dosis_ke,
       given_date, no_batch, tempat, tempat_keterangan, pemberi,
       row_version, created_at, updated_at
FROM clinical.person_riwayat_vaksin
WHERE person_id = $1 AND antigen_slug = $2
ORDER BY given_date DESC;

-- name: GetImmunizationSchedule :many
SELECT id, antigen_slug, label, dosis_ke, kategori, usia_min_bln, usia_maks_bln,
       interval_hari_dari_dosis_sebelumnya, masa_perlindungan_tahun,
       source_name, source_version, source_url, downloaded_at
FROM ref.immunization_schedule
WHERE antigen_slug = $1 AND kategori = $2
ORDER BY dosis_ke;
