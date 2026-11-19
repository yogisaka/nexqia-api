-- Plan 2026-11-19-gizi-balita-1-be Task 1 (spec 2026-11-19-gizi-balita-revisi-design
-- §3–§4, §6): konteks "bayi kecil" (BBL + usia gestasi), trigger snapshot v3
-- dengan koreksi usia prematur, dan section recall MPASI di template
-- ases_gizi_adime.

-- 1. Tabel konteks antropometri bayi kecil (spec §3.1; idiom RLS tenant
--    000046:31 / 000074:150 — NULLIF wajib agar setting kosong = no-row,
--    bukan error 22P02).
CREATE TABLE clinical.person_growth_context (
    id               uuid PRIMARY KEY DEFAULT gen_random_uuid(),
    company_id       uuid NOT NULL,
    person_id        uuid NOT NULL,
    bbl_kg           numeric(4,2) NOT NULL CHECK (bbl_kg > 0 AND bbl_kg < 6),
    usia_gestasi_mgg smallint NOT NULL CHECK (usia_gestasi_mgg BETWEEN 20 AND 45),
    source_ref       text NOT NULL DEFAULT 'manual',
    recorded_at      timestamptz NOT NULL,
    recorded_by      uuid NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    UNIQUE (company_id, person_id)
);
CREATE INDEX idx_person_growth_context_company_person
    ON clinical.person_growth_context (company_id, person_id);
ALTER TABLE clinical.person_growth_context ENABLE ROW LEVEL SECURITY;
CREATE POLICY person_growth_context_isolation ON clinical.person_growth_context
    USING (company_id = NULLIF(current_setting('app.current_company_id', true), '')::uuid);

-- 2. Trigger snapshot v3: koreksi usia prematur (spec §3.2). Basis literal
--    000105_gizi_anak.up.sql:126-257; delta: bila konteks bayi kecil ada
--    (usia gestasi < 37 mgg) dan usia < 731 hari, umur efektif = umur kronologis
--    dikurangi (40 mgg − usia gestasi) dalam minggu; guard tanpa konteks /
--    aterm >= 37 mgg / usia >= 731 hari = PERSIS perilaku v2.
CREATE OR REPLACE FUNCTION clinical.fn_anthropometry_snapshot() RETURNS trigger AS $fn$
DECLARE
    r  ref.growth_curve%ROWTYPE;
    b  ref.growth_status_band%ROWTYPE;
    zz numeric;
    st jsonb := '{}'::jsonb;
    cv text;
    am numeric; -- age_months = floor(age_days/30.4375)
    bd text;    -- domain band: balita (<=60 bln) / anak (61-228 bln)
    v_gest smallint;
    v_age_eff_days integer;
BEGIN
    NEW.imt := NULL; NEW.imt_kategori := NULL; NEW.curve_version := NULL;
    NEW.zscore_bbu := NULL; NEW.zscore_tbu := NULL; NEW.zscore_bbtb := NULL;
    NEW.zscore_imtu := NULL; NEW.zscore_lku := NULL; NEW.zscore_lila := NULL;
    NEW.status := '{}'::jsonb;
    SELECT usia_gestasi_mgg INTO v_gest
    FROM clinical.person_growth_context g
    WHERE g.person_id = NEW.person_id AND g.company_id = NEW.company_id;
    v_age_eff_days := CASE
        WHEN v_gest IS NOT NULL AND v_gest < 37 AND NEW.age_days < 731
            THEN GREATEST(NEW.age_days - ((40 - v_gest) * 7), 0)
        ELSE NEW.age_days END;
    am := NULL;
    IF v_age_eff_days IS NOT NULL THEN
        am := floor(v_age_eff_days / 30.4375);
    END IF;
    IF NEW.bb_kg IS NOT NULL AND NEW.tb_cm IS NOT NULL THEN
        NEW.imt := round(NEW.bb_kg / power(NEW.tb_cm / 100, 2), 1);
        NEW.imt_kategori := CASE
            WHEN NEW.imt < 18.5 THEN 'kurus'
            WHEN NEW.imt < 23 THEN 'normal'
            WHEN NEW.imt < 25 THEN 'bb_lebih'
            ELSE 'obesitas' END;  -- KMK 509/2025 (docs/30 §11.7), batas atas tertutup
        IF am IS NOT NULL AND am <= 228 THEN
            NEW.imt_kategori := NULL;  -- <19 th: kategori IMT dewasa tidak tampil
        END IF;
    END IF;
    bd := CASE WHEN am IS NULL OR am <= 60 THEN 'balita' ELSE 'anak' END;
    -- BB/U: hanya <= 120 bln (WHO CGS 0-5 th); baris ref gender sama,
    -- x terdekat (tie: x lebih kecil).
    IF am IS NOT NULL AND am <= 120 AND NEW.bb_kg IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'bbu' AND gender = NEW.gender AND x_kind = 'age_days'
        ORDER BY abs(x - v_age_eff_days), x LIMIT 1;
        IF found THEN
            zz := CASE WHEN r.l = 0 THEN ln(NEW.bb_kg / r.m) / r.s
                       ELSE (power(NEW.bb_kg / r.m, r.l) - 1) / (r.l * r.s) END;
            NEW.zscore_bbu := round(zz, 2); cv := r.source_version;
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'bbu' AND basis = 'z' AND domain = bd
              AND (low IS NULL OR low <= NEW.zscore_bbu) AND (high IS NULL OR NEW.zscore_bbu < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('bbu', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    -- TB/U: <= 228 bln (5-18 th WHO GR 2007).
    IF am IS NOT NULL AND am <= 228 AND NEW.tb_cm IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'tbu' AND gender = NEW.gender AND x_kind = 'age_days'
        ORDER BY abs(x - v_age_eff_days), x LIMIT 1;
        IF found THEN
            zz := CASE WHEN r.l = 0 THEN ln(NEW.tb_cm / r.m) / r.s
                       ELSE (power(NEW.tb_cm / r.m, r.l) - 1) / (r.l * r.s) END;
            NEW.zscore_tbu := round(zz, 2); cv := r.source_version;
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'tbu' AND basis = 'z' AND domain = bd
              AND (low IS NULL OR low <= NEW.zscore_tbu) AND (high IS NULL OR NEW.zscore_tbu < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('tbu', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    -- BB/TB: guard lama age_days <= 2192 (band balita, tanpa pilah domain).
    IF v_age_eff_days IS NOT NULL AND v_age_eff_days <= 2192 AND NEW.bb_kg IS NOT NULL AND NEW.tb_cm IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'bbtb' AND gender = NEW.gender AND x_kind = 'height_cm'
        ORDER BY abs(x - NEW.tb_cm), x LIMIT 1;
        IF found THEN
            zz := CASE WHEN r.l = 0 THEN ln(NEW.bb_kg / r.m) / r.s
                       ELSE (power(NEW.bb_kg / r.m, r.l) - 1) / (r.l * r.s) END;
            NEW.zscore_bbtb := round(zz, 2); cv := r.source_version;
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'bbtb' AND basis = 'z'
              AND (low IS NULL OR low <= NEW.zscore_bbtb) AND (high IS NULL OR NEW.zscore_bbtb < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('bbtb', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    -- IMT/U: <= 228 bln — dipindah keluar guard 2192 agar 61-228 bln (5-18 th)
    -- tetap dihitung (Task 2; sebelumnya nested di blok 2192).
    IF am IS NOT NULL AND am <= 228 AND NEW.imt IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'imtu' AND gender = NEW.gender AND x_kind = 'age_days'
        ORDER BY abs(x - v_age_eff_days), x LIMIT 1;
        IF found THEN
            zz := CASE WHEN r.l = 0 THEN ln(NEW.imt / r.m) / r.s
                       ELSE (power(NEW.imt / r.m, r.l) - 1) / (r.l * r.s) END;
            NEW.zscore_imtu := round(zz, 2); cv := r.source_version;
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'imtu' AND basis = 'z' AND domain = bd
              AND (low IS NULL OR low <= NEW.zscore_imtu) AND (high IS NULL OR NEW.zscore_imtu < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('imtu', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    -- LK/U: guard lama age_days <= 2192 (band balita).
    IF v_age_eff_days IS NOT NULL AND v_age_eff_days <= 2192 AND (NEW.extras ->> 'lk_cm') IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'lku' AND gender = NEW.gender AND x_kind = 'age_days'
        ORDER BY abs(x - v_age_eff_days), x LIMIT 1;
        IF found THEN
            zz := CASE WHEN r.l = 0 THEN ln((NEW.extras->>'lk_cm')::numeric / r.m) / r.s
                       ELSE (power((NEW.extras->>'lk_cm')::numeric / r.m, r.l) - 1) / (r.l * r.s) END;
            NEW.zscore_lku := round(zz, 2); cv := r.source_version;
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'lku' AND basis = 'z'
              AND (low IS NULL OR low <= NEW.zscore_lku) AND (high IS NULL OR NEW.zscore_lku < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('lku', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    IF (NEW.extras ->> 'lila_cm') IS NOT NULL THEN
        IF v_age_eff_days IS NOT NULL AND v_age_eff_days <= 2192 THEN
            SELECT * INTO r FROM ref.growth_curve
            WHERE indicator = 'lila' AND gender = NEW.gender AND x_kind = 'age_days'
            ORDER BY abs(x - v_age_eff_days), x LIMIT 1;
            IF found THEN
                zz := CASE WHEN r.l = 0 THEN ln((NEW.extras->>'lila_cm')::numeric / r.m) / r.s
                           ELSE (power((NEW.extras->>'lila_cm')::numeric / r.m, r.l) - 1) / (r.l * r.s) END;
                NEW.zscore_lila := round(zz, 2); cv := r.source_version;
            END IF;
        END IF;
        -- Band LiLA anak (basis cm) hanya usia 6-59 bulan (Buku KIA; ASUMSI 183..1795 hari).
        IF v_age_eff_days IS NOT NULL AND v_age_eff_days >= 183 AND v_age_eff_days <= 1795 THEN
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'lila_cm' AND basis = 'cm'
              AND (low IS NULL OR low <= (NEW.extras->>'lila_cm')::numeric)
              AND (high IS NULL OR (NEW.extras->>'lila_cm')::numeric < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('lila_cm', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    -- Koreksi prematur tercatat: umur efektif + bulan koreksi hanya bila beda
    -- dari umur kronologis (spec §3.2).
    IF v_age_eff_days <> NEW.age_days THEN
        st := st || jsonb_build_object(
            'age_eff_days', v_age_eff_days,
            'usia_koreksi_bulan', floor(v_age_eff_days / 30.4375));
    END IF;
    NEW.status := st;
    NEW.curve_version := cv;
    RETURN NEW;
END
$fn$ LANGUAGE plpgsql;

-- 3. Section recall MPASI (spec §3.3, FR-BT-07): tulis ulang literal object
--    sections penuh (pola 000103 — DILARANG jsonb ||/- shallow-merge karena
--    menimpa array "sections"); 4 section existing 000089:78-91 + 3 section baru.
UPDATE core.template_clinical_form SET sections = '{"sections":[
        {"key":"assessment","label":"A — Assessment (antropometri/klinis/dietary)","kind":"textarea","soft_required":true},
        {"key":"diagnosis_gizi","label":"D — Diagnosis gizi","kind":"select","options":[
            {"value":"GD-01","label":"Asupan makan kurang dari kebutuhan"},
            {"value":"GD-02","label":"Asupan makan berlebih dari kebutuhan"},
            {"value":"GD-03","label":"Perubahan berat badan tidak diinginkan"},
            {"value":"GD-04","label":"Ketidakadekuatan zat gizi makro (protein/energi)"},
            {"value":"GD-05","label":"Ketidakadekuatan zat gizi mikro"},
            {"value":"GD-06","label":"Ketidakseimbangan cairan"},
            {"value":"GD-07","label":"Kesulitan menelan/mengunyah (modifikasi tekstur)"},
            {"value":"GD-08","label":"Risiko defisit zat gizi terkait penyakit"},
            {"value":"GD-99","label":"Lainnya (jelaskan pada intervensi)"}]},
        {"key":"intervention","label":"I — Intervention (rencana diet & edukasi)","kind":"textarea","soft_required":true},
        {"key":"monitoring_evaluation","label":"M/E — Rencana monitoring & evaluasi","kind":"textarea"},
        {"key":"recall_mpasi_kelompok","label":"Recall MPASI 24 jam — kelompok makanan (MDD)","kind":"multiselect","options":[
            {"value":"asi","label":"ASI / susu ibu"},
            {"value":"pokok","label":"Makanan pokok"},
            {"value":"nabati","label":"Kacang-kacangan / biji"},
            {"value":"dairy","label":"Susu / produk susu"},
            {"value":"hewani","label":"Daging / ikan / hati"},
            {"value":"telur","label":"Telur"},
            {"value":"sayur_a","label":"Sayur vitamin A (hijau/oranye)"},
            {"value":"buah_lain","label":"Buah / sayur lain"}]},
        {"key":"recall_mpasi_status","label":"Status ASI / susu (24 jam)","kind":"select","options":[
            {"value":"eksklusif","label":"ASI eksklusif"},
            {"value":"campuran","label":"ASI + makanan/minuman lain (campuran)"},
            {"value":"formula","label":"Susu formula"},
            {"value":"tidak","label":"Tidak mendapat ASI / susu ibu"}]},
        {"key":"recall_mpasi_mmf","label":"MMF — frekuensi makan 24 jam","kind":"repeatable","fields":[
            {"key":"item","label":"Jenis makan","type":"select","options":[
                {"value":"utama","label":"Makanan utama"},
                {"value":"selingan","label":"Selingan"}]},
            {"key":"nilai","label":"Frekuensi (kali)","type":"number"}]}]}'::jsonb
WHERE form_type = 'ases_gizi_adime';
