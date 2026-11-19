-- Plan 2026-11-19-gizi-balita-1-be Task 1: DOWN migration 000106_gizi_balita.
-- Pulihkan fungsi snapshot ke v2 (literal 000105_gizi_anak.up.sql:126-257),
-- sections ases_gizi_adime ke literal 4 section 000089:78-91 (pola 000103
-- DOWN — tulis ulang literal object penuh), lalu drop tabel konteks.

CREATE OR REPLACE FUNCTION clinical.fn_anthropometry_snapshot() RETURNS trigger AS $fn$
DECLARE
    r  ref.growth_curve%ROWTYPE;
    b  ref.growth_status_band%ROWTYPE;
    zz numeric;
    st jsonb := '{}'::jsonb;
    cv text;
    am numeric; -- age_months = floor(age_days/30.4375)
    bd text;    -- domain band: balita (<=60 bln) / anak (61-228 bln)
BEGIN
    NEW.imt := NULL; NEW.imt_kategori := NULL; NEW.curve_version := NULL;
    NEW.zscore_bbu := NULL; NEW.zscore_tbu := NULL; NEW.zscore_bbtb := NULL;
    NEW.zscore_imtu := NULL; NEW.zscore_lku := NULL; NEW.zscore_lila := NULL;
    NEW.status := '{}'::jsonb;
    am := NULL;
    IF NEW.age_days IS NOT NULL THEN
        am := floor(NEW.age_days / 30.4375);
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
        ORDER BY abs(x - NEW.age_days), x LIMIT 1;
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
        ORDER BY abs(x - NEW.age_days), x LIMIT 1;
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
    IF NEW.age_days IS NOT NULL AND NEW.age_days <= 2192 AND NEW.bb_kg IS NOT NULL AND NEW.tb_cm IS NOT NULL THEN
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
        ORDER BY abs(x - NEW.age_days), x LIMIT 1;
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
    IF NEW.age_days IS NOT NULL AND NEW.age_days <= 2192 AND (NEW.extras ->> 'lk_cm') IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'lku' AND gender = NEW.gender AND x_kind = 'age_days'
        ORDER BY abs(x - NEW.age_days), x LIMIT 1;
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
        IF NEW.age_days IS NOT NULL AND NEW.age_days <= 2192 THEN
            SELECT * INTO r FROM ref.growth_curve
            WHERE indicator = 'lila' AND gender = NEW.gender AND x_kind = 'age_days'
            ORDER BY abs(x - NEW.age_days), x LIMIT 1;
            IF found THEN
                zz := CASE WHEN r.l = 0 THEN ln((NEW.extras->>'lila_cm')::numeric / r.m) / r.s
                           ELSE (power((NEW.extras->>'lila_cm')::numeric / r.m, r.l) - 1) / (r.l * r.s) END;
                NEW.zscore_lila := round(zz, 2); cv := r.source_version;
            END IF;
        END IF;
        -- Band LiLA anak (basis cm) hanya usia 6-59 bulan (Buku KIA; ASUMSI 183..1795 hari).
        IF NEW.age_days IS NOT NULL AND NEW.age_days >= 183 AND NEW.age_days <= 1795 THEN
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'lila_cm' AND basis = 'cm'
              AND (low IS NULL OR low <= (NEW.extras->>'lila_cm')::numeric)
              AND (high IS NULL OR (NEW.extras->>'lila_cm')::numeric < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('lila_cm', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    NEW.status := st;
    NEW.curve_version := cv;
    RETURN NEW;
END
$fn$ LANGUAGE plpgsql;

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
        {"key":"monitoring_evaluation","label":"M/E — Rencana monitoring & evaluasi","kind":"textarea"}]}'::jsonb
WHERE form_type = 'ases_gizi_adime';

DROP TABLE clinical.person_growth_context;
