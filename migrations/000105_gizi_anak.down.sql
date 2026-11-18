-- Plan 2026-11-18-anak-a1-1-be Task 1 (spec 2026-11-18-anak-gizi-a1-design
-- §3–§6, §8): DOWN migration 000105_gizi_anak. CHECK instrument sengaja TIDAK
-- direstore ke 4 nilai (pola 000104: baris 'stamp' historis dipertahankan —
-- restore akan gagal validasi). CHECK profile dikembalikan ke 5 nilai HANYA
-- bila tidak ada baris 'anak'.

-- Restore fungsi fn_anthropometry_snapshot v1 (verbatim dari
-- 000097_gizi_fondasi.up.sql :194-311) SEBELUM kolom domain dihapus: UP
-- mengganti fungsi ini ke v2 yang merujuk growth_status_band.domain — tanpa
-- restore, INSERT/UPDATE antropometri gagal 42703 setelah migrate-down.
CREATE OR REPLACE FUNCTION clinical.fn_anthropometry_snapshot() RETURNS trigger AS $fn$
DECLARE
    r  ref.growth_curve%ROWTYPE;
    b  ref.growth_status_band%ROWTYPE;
    zz numeric;
    st jsonb := '{}'::jsonb;
    cv text;
BEGIN
    NEW.imt := NULL; NEW.imt_kategori := NULL; NEW.curve_version := NULL;
    NEW.zscore_bbu := NULL; NEW.zscore_tbu := NULL; NEW.zscore_bbtb := NULL;
    NEW.zscore_imtu := NULL; NEW.zscore_lku := NULL; NEW.zscore_lila := NULL;
    NEW.status := '{}'::jsonb;
    IF NEW.bb_kg IS NOT NULL AND NEW.tb_cm IS NOT NULL THEN
        NEW.imt := round(NEW.bb_kg / power(NEW.tb_cm / 100, 2), 1);
        NEW.imt_kategori := CASE
            WHEN NEW.imt < 18.5 THEN 'kurus'
            WHEN NEW.imt < 23 THEN 'normal'
            WHEN NEW.imt < 25 THEN 'bb_lebih'
            ELSE 'obesitas' END;  -- KMK 509/2025 (docs/30 §11.7), batas atas tertutup
    END IF;
    -- z per indikator: HANYA domain anak — baris ref dipilih bila age_days <= 2192
    -- (0-5 th, domain WHO CGS + FR-GZ-14; di luar itu semua z NULL — dewasa/bumil
    -- tidak pernah mendapat z anak). Baris gender sama, x terdekat (tie: x lebih kecil).
    IF NEW.age_days IS NOT NULL AND NEW.age_days <= 2192 AND NEW.bb_kg IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'bbu' AND gender = NEW.gender AND x_kind = 'age_days'
        ORDER BY abs(x - NEW.age_days), x LIMIT 1;
        IF found THEN
            zz := CASE WHEN r.l = 0 THEN ln(NEW.bb_kg / r.m) / r.s
                       ELSE (power(NEW.bb_kg / r.m, r.l) - 1) / (r.l * r.s) END;
            NEW.zscore_bbu := round(zz, 2); cv := r.source_version;
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'bbu' AND basis = 'z'
              AND (low IS NULL OR low <= NEW.zscore_bbu) AND (high IS NULL OR NEW.zscore_bbu < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('bbu', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
    IF NEW.age_days IS NOT NULL AND NEW.age_days <= 2192 AND NEW.tb_cm IS NOT NULL THEN
        SELECT * INTO r FROM ref.growth_curve
        WHERE indicator = 'tbu' AND gender = NEW.gender AND x_kind = 'age_days'
        ORDER BY abs(x - NEW.age_days), x LIMIT 1;
        IF found THEN
            zz := CASE WHEN r.l = 0 THEN ln(NEW.tb_cm / r.m) / r.s
                       ELSE (power(NEW.tb_cm / r.m, r.l) - 1) / (r.l * r.s) END;
            NEW.zscore_tbu := round(zz, 2); cv := r.source_version;
            SELECT * INTO b FROM ref.growth_status_band
            WHERE indicator = 'tbu' AND basis = 'z'
              AND (low IS NULL OR low <= NEW.zscore_tbu) AND (high IS NULL OR NEW.zscore_tbu < high) LIMIT 1;
            IF found THEN st := st || jsonb_build_object('tbu', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
        END IF;
    END IF;
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
        IF NEW.age_days IS NOT NULL THEN
            SELECT * INTO r FROM ref.growth_curve
            WHERE indicator = 'imtu' AND gender = NEW.gender AND x_kind = 'age_days'
            ORDER BY abs(x - NEW.age_days), x LIMIT 1;
            IF found THEN
                zz := CASE WHEN r.l = 0 THEN ln(NEW.imt / r.m) / r.s
                           ELSE (power(NEW.imt / r.m, r.l) - 1) / (r.l * r.s) END;
                NEW.zscore_imtu := round(zz, 2); cv := r.source_version;
                SELECT * INTO b FROM ref.growth_status_band
                WHERE indicator = 'imtu' AND basis = 'z'
                  AND (low IS NULL OR low <= NEW.zscore_imtu) AND (high IS NULL OR NEW.zscore_imtu < high) LIMIT 1;
                IF found THEN st := st || jsonb_build_object('imtu', jsonb_build_object('code', b.code, 'label', b.label, 'risk', b.risk)); END IF;
            END IF;
        END IF;
    END IF;
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

DELETE FROM core.template_clinical_form WHERE form_type='ases_gizi_stamp';
DELETE FROM core.template WHERE kind='clinical_form' AND code='ases_gizi_stamp';

-- Band anak 5–18: hapus SATU baris per tuple anak, HANYA dari kolom domain
-- 'anak'. Dua tuple anak identik 7 kolom dengan baris balita dari seed
-- 000097 ('Gizi baik' bbu -2/+1 dan 'Sangat pendek (stunting berat)' tbu
-- NULL/-3) — filter domain memastikan instance balita tidak tersentuh,
-- sehingga ctid LIMIT 1 hanya menghapus instance anak.
DO $$
DECLARE v record;
BEGIN
    FOR v IN SELECT * FROM (VALUES
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
    ) t(indicator, basis, low, high, code, label, risk)
    LOOP
        DELETE FROM ref.growth_status_band b
        WHERE b.ctid = (
            SELECT c.ctid FROM ref.growth_status_band c
            WHERE c.indicator = v.indicator AND c.basis = v.basis
              AND c.low IS NOT DISTINCT FROM v.low AND c.high IS NOT DISTINCT FROM v.high
              AND c.code = v.code AND c.label = v.label AND c.risk = v.risk
              AND c.domain = 'anak'
            LIMIT 1);
    END LOOP;
END $$;

-- CHECK profile kembali ke 5 nilai hanya bila tidak ada baris 'anak'.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM clinical.nutrition_episode WHERE profile='anak')
       OR EXISTS (SELECT 1 FROM clinical.nutrition_profile_change
                  WHERE from_profile='anak' OR to_profile='anak') THEN
        RAISE EXCEPTION 'masih ada baris profil anak — CHECK profile tidak bisa dikembalikan ke 5 nilai';
    END IF;
END $$;

ALTER TABLE clinical.nutrition_episode
  DROP CONSTRAINT nutrition_episode_profile_check;
ALTER TABLE clinical.nutrition_episode
  ADD CONSTRAINT nutrition_episode_profile_check
  CHECK (profile IN ('dewasa','lansia','bumil','menyusui','balita'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT nutrition_profile_change_from_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_from_profile_check
  CHECK (from_profile IN ('dewasa','lansia','bumil','menyusui','balita'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT nutrition_profile_change_to_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_to_profile_check
  CHECK (to_profile IN ('dewasa','lansia','bumil','menyusui','balita'));

-- Kolom domain (up.sql ADD COLUMN IF NOT EXISTS): drop tanpa guard data karena
-- baris 'anak' sudah dihapus di atas — tidak ada UNIQUE yang mengikat kolom ini.
ALTER TABLE ref.growth_status_band
  DROP COLUMN IF EXISTS domain;
