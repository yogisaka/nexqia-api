-- Plan 2026-11-18-anak-a1-1-be Task 1 (spec 2026-11-18-anak-gizi-a1-design
-- §3–§6, §8): profil gizi ke-6 'anak' (5–18 th), band status gizi
-- ref.growth_status_band usia 5–18 (label verbatim Permenkes 2/2020 —
-- docs/agent/ref/cppt/gizi/permenkes2-2020.txt; IMT/U 5–18 verbatim tabel
-- kategori, BB/U & TB/U 5–18 tidak punya tabel kategori terpisah → label
-- draft M1), template skrining STAMP (footer atribusi verbatim), CHECK
-- instrument nutrition_screening_result diperluas dengan 'stamp'.

-- 1. CHECK profile diperluas (nama constraint generated 000097 dipertahankan,
--    pola 000104:30-34 — DROP IF EXISTS lalu ADD nama sama).
ALTER TABLE clinical.nutrition_episode
  DROP CONSTRAINT IF EXISTS nutrition_episode_profile_check;
ALTER TABLE clinical.nutrition_episode
  ADD CONSTRAINT nutrition_episode_profile_check
  CHECK (profile IN ('dewasa','lansia','bumil','menyusui','balita','anak'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT IF EXISTS nutrition_profile_change_from_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_from_profile_check
  CHECK (from_profile IN ('dewasa','lansia','bumil','menyusui','balita','anak'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT IF EXISTS nutrition_profile_change_to_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_to_profile_check
  CHECK (to_profile IN ('dewasa','lansia','bumil','menyusui','balita','anak'));

-- 2. Band status gizi anak 5–18 (14 baris; low inklusif, high eksklusif;
--    domain kolom pilihan trigger (Task 2): baris lama 000097 = 'balita'
--    (DEFAULT), baris baru = 'anak'.
--    cutoff Permenkes 2/2020 usia 5–18 — IMT/U verbatim; bbu: >+2 obesitas;
--    tbu: >+2 tinggi). Domain usia implisit (5–18) — dipilah trigger per
--    age_months (Task 2). Baris balita existing TIDAK berubah.
ALTER TABLE ref.growth_status_band
  ADD COLUMN IF NOT EXISTS domain text NOT NULL DEFAULT 'balita';

INSERT INTO ref.growth_status_band (indicator, basis, low, high, code, label, risk, domain) VALUES
    ('bbu','z',NULL,-3,'gizi_buruk','Gizi buruk (kurus berat)','tinggi','anak'),
    ('bbu','z',-3,-2,'gizi_kurang','Gizi kurang (kurus)','sedang','anak'),
    ('bbu','z',-2,1,'gizi_baik','Gizi baik','rendah','anak'),
    ('bbu','z',1,2,'gizi_lebih','Gizi lebih (gemuk)','sedang','anak'),
    ('bbu','z',2,NULL,'obesitas','Obesitas','tinggi','anak'),
    ('tbu','z',NULL,-3,'sangat_pendek','Sangat pendek (stunting berat)','tinggi','anak'),
    ('tbu','z',-3,-2,'stunting','Pendek (stunting)','sedang','anak'),
    ('tbu','z',-2,2,'normal','Normal','rendah','anak'),
    ('tbu','z',2,NULL,'tinggi','Tinggi','rendah','anak'),
    ('imtu','z',NULL,-3,'kurus_berat','Gizi buruk (severely thinness)','tinggi','anak'),
    ('imtu','z',-3,-2,'kurus','Gizi kurang (thinness)','sedang','anak'),
    ('imtu','z',-2,1,'normal','Gizi baik (normal)','rendah','anak'),
    ('imtu','z',1,2,'gemuk','Gizi lebih (overweight)','sedang','anak'),
    ('imtu','z',2,NULL,'obesitas','Obesitas (obese)','tinggi','anak');

DO $$
DECLARE n integer;
BEGIN
    SELECT count(*) INTO n
    FROM (VALUES
        ('bbu','z',NULL::numeric,-3::numeric,'gizi_buruk','Gizi buruk (kurus berat)','tinggi'),
        ('bbu','z',-3::numeric,-2::numeric,'gizi_kurang','Gizi kurang (kurus)','sedang'),
        ('bbu','z',-2::numeric,1::numeric,'gizi_baik','Gizi baik','rendah'),
        ('bbu','z',1::numeric,2::numeric,'gizi_lebih','Gizi lebih (gemuk)','sedang'),
        ('bbu','z',2::numeric,NULL::numeric,'obesitas','Obesitas','tinggi'),
        ('tbu','z',NULL::numeric,-3::numeric,'sangat_pendek','Sangat pendek (stunting berat)','tinggi'),
        ('tbu','z',-3::numeric,-2::numeric,'stunting','Pendek (stunting)','sedang'),
        ('tbu','z',-2::numeric,2::numeric,'normal','Normal','rendah'),
        ('tbu','z',2::numeric,NULL::numeric,'tinggi','Tinggi','rendah'),
        ('imtu','z',NULL::numeric,-3::numeric,'kurus_berat','Gizi buruk (severely thinness)','tinggi'),
        ('imtu','z',-3::numeric,-2::numeric,'kurus','Gizi kurang (thinness)','sedang'),
        ('imtu','z',-2::numeric,1::numeric,'normal','Gizi baik (normal)','rendah'),
        ('imtu','z',1::numeric,2::numeric,'gemuk','Gizi lebih (overweight)','sedang'),
        ('imtu','z',2::numeric,NULL::numeric,'obesitas','Obesitas (obese)','tinggi')
    ) v(indicator, basis, low, high, code, label, risk)
    JOIN ref.growth_status_band b
      ON b.indicator = v.indicator AND b.basis = v.basis AND b.domain = 'anak'
     AND b.low IS NOT DISTINCT FROM v.low AND b.high IS NOT DISTINCT FROM v.high
     AND b.code = v.code AND b.label = v.label AND b.risk = v.risk;
    IF n <> 14 THEN
        RAISE EXCEPTION 'seed band anak 5-18 tidak lengkap: % dari 14', n;
    END IF;
END $$;

-- 3. Template skrining STAMP (pola 000104:6-22 / 000098:6-14). Step 1–2 radio
--    dari payload; step 3 (berat/tinggi vs grafik) dan risiko total dihitung
--    server (hook gizi_care, Task 3). Footer atribusi verbatim di description.
INSERT INTO core.template (kind, code, name, description, scope, usage) VALUES
    ('clinical_form','ases_gizi_stamp','Skrining Risiko Gizi Anak (STAMP)',
     'STAMP: langkah 1 diagnosis, langkah 2 asupan nutrisi, langkah 3 antropometri (server); risiko total >=4 tinggi, 2-3 sedang, 0-1 rendah. STAMP© — Screening Tool for the Assessment of Malnutrition in Paediatrics (stampscreeningtool.org)','platform','insert');

INSERT INTO core.template_clinical_form (template_id, form_type, category, sections)
SELECT t.id, 'ases_gizi_stamp', 'gizi', '{
  "sections":[
    {"key":"stamp_diagnosis","label":"Langkah 1 — Diagnosis","kind":"select","options":[
      {"value":"3","label":"Definitely — ada implikasi gizi pasti"},
      {"value":"2","label":"Possibly — mungkin ada implikasi"},
      {"value":"0","label":"No — tidak ada"}]},
    {"key":"stamp_asupan","label":"Langkah 2 — Asupan nutrisi","kind":"select","options":[
      {"value":"3","label":"Tidak ada asupan"},
      {"value":"2","label":"Asupan menurun / buruk"},
      {"value":"0","label":"Tidak berubah / baik"}]},
    {"key":"stamp_step3_sumber","label":"Langkah 3 — Berat & tinggi badan vs grafik","kind":"readonly","value":"Dihitung server dari antropometri terakhir"},
    {"key":"stamp_total","label":"Risiko total","kind":"readonly","value":"Dihitung server: >=4 tinggi, 2-3 sedang, 0-1 rendah"}
  ]}'::jsonb
FROM core.template t
WHERE t.kind='clinical_form' AND t.code='ases_gizi_stamp';

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM core.template_clinical_form WHERE form_type='ases_gizi_stamp') THEN
    RAISE EXCEPTION 'template ases_gizi_stamp gagal dibuat (data acuan core.template hilang)';
  END IF;
END $$;

-- 4. CHECK instrument diperluas dengan 'stamp'.
ALTER TABLE clinical.nutrition_screening_result
  DROP CONSTRAINT IF EXISTS nutrition_screening_result_instrument_check;
ALTER TABLE clinical.nutrition_screening_result
  ADD CONSTRAINT nutrition_screening_result_instrument_check
  CHECK (instrument IN ('mst','mnasf','rules','pemicu_bumil','stamp'));

-- 5. Trigger snapshot v2 (Task 2): sumber z per-indikator via
--    age_months = floor(age_days/30.4375) — bbu hanya <= 120 bln, tbu/imtu
--    <= 228 bln (61–228 = WHO GR 2007), bbtb/lku/lila tetap guard lama
--    age_days <= 2192. Band bbu/tbu/imtu dipilah ref.growth_status_band.domain
--    ('balita' <= 60 bln, 'anak' 61–228). imt_kategori := NULL untuk
--    <= 228 bln (<19 th, D-KA-04). zwrite & interpolasi LMS mekanisme lama.
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
