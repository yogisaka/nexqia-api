BEGIN;

-- Gizi fondasi (spec 2026-11-13-gizi-nutrition-care-spec §4.1). Band status = RISET Permenkes 2/2020
-- (verifikasi review; band lila_cm terkonfirmasi Buku KIA 2024).

-- 1. Referensi kurva pertumbuhan (data via cmd/import-growth-curve, BUKAN di migration).
CREATE SCHEMA IF NOT EXISTS ref;
GRANT USAGE ON SCHEMA ref TO app_runtime;
CREATE TABLE ref.growth_curve (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    indicator      text NOT NULL CHECK (indicator IN ('bbu','tbu','bbtb','imtu','lku','lila')),
    gender         text NOT NULL CHECK (gender IN ('laki_laki','perempuan')),
    x_kind         text NOT NULL CHECK (x_kind IN ('age_days','height_cm')),
    x              numeric(8,2) NOT NULL CHECK (x > 0),
    l              numeric(10,6) NOT NULL,
    m              numeric(10,4) NOT NULL,
    s              numeric(10,6) NOT NULL,
    source_name    text NOT NULL,
    source_version text NOT NULL,
    source_url     text NOT NULL,
    downloaded_at  timestamptz NOT NULL,
    UNIQUE (indicator, gender, x, source_version)
);
GRANT SELECT ON ref.growth_curve TO app_runtime;

-- 2. Band interpretasi status (Permenkes 2/2020; low inklusif, high EKSKLUSIF).
CREATE TABLE ref.growth_status_band (
    id        uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    indicator text NOT NULL CHECK (indicator IN ('bbu','tbu','bbtb','imtu','lku','lila_cm')),
    basis     text NOT NULL CHECK (basis IN ('z','cm')),
    low       numeric(7,2),
    high      numeric(7,2),
    code      text NOT NULL,
    label     text NOT NULL,
    risk      text NOT NULL CHECK (risk IN ('rendah','sedang','tinggi'))
);
GRANT SELECT ON ref.growth_status_band TO app_runtime;
INSERT INTO ref.growth_status_band (indicator, basis, low, high, code, label, risk) VALUES
    ('bbu','z',NULL,-3,'gizi_buruk','Gizi buruk','tinggi'),
    ('bbu','z',-3,-2,'gizi_kurang','Gizi kurang','sedang'),
    ('bbu','z',-2,1,'gizi_baik','Gizi baik','rendah'),
    ('bbu','z',1,2,'risiko_gizi_lebih','Risiko gizi lebih','sedang'),
    ('bbu','z',2,3,'gizi_lebih','Gizi lebih','sedang'),
    ('bbu','z',3,NULL,'obesitas','Obesitas','tinggi'),
    ('tbu','z',NULL,-3,'sangat_pendek','Sangat pendek (stunting berat)','tinggi'),
    ('tbu','z',-3,-2,'stunting','Stunting','sedang'),
    ('tbu','z',-2,3,'normal','Normal','rendah'),
    ('tbu','z',3,NULL,'tinggi','Tinggi','rendah'),
    ('bbtb','z',NULL,-3,'wasting_berat','Wasting berat','tinggi'),
    ('bbtb','z',-3,-2,'wasting','Wasting','sedang'),
    ('bbtb','z',-2,2,'normal','Normal','rendah'),
    ('bbtb','z',2,3,'gizi_lebih','Gizi lebih','sedang'),
    ('bbtb','z',3,NULL,'obesitas','Obesitas','tinggi'),
    ('imtu','z',NULL,-3,'wasting_berat','Wasting berat','tinggi'),
    ('imtu','z',-3,-2,'gizi_kurang','Gizi kurang','sedang'),
    ('imtu','z',-2,1,'gizi_baik','Gizi baik','rendah'),
    ('imtu','z',1,2,'gizi_lebih','Gizi lebih','sedang'),
    ('imtu','z',2,NULL,'obesitas','Obesitas','tinggi'),
    ('lku','z',NULL,-3,'mikrosefali','Mikrosefali','tinggi'),
    ('lku','z',-3,-2,'waspada_mikro','Waspada mikrosefali','sedang'),
    ('lku','z',-2,2,'normal','Normal','rendah'),
    ('lku','z',2,3,'waspada_makro','Waspada makrosefali','sedang'),
    ('lku','z',3,NULL,'makrosefali','Makrosefali','tinggi'),
    ('lila_cm','cm',NULL,11.5,'gizi_buruk','Gizi buruk','tinggi'),
    ('lila_cm','cm',11.5,12.5,'gizi_kurang','Gizi kurang','sedang'),
    ('lila_cm','cm',12.5,NULL,'gizi_baik','Gizi baik','rendah');

-- 3. Antropometri hybrid (D4: satu sumber pengukuran per person; snapshot audit-preservatif A2).
CREATE TABLE clinical.anthropometry (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    person_id    uuid NOT NULL REFERENCES core.person(id),
    admission_id uuid REFERENCES operations.admission(id),
    measured_at  timestamptz NOT NULL,
    gender       text NOT NULL CHECK (gender IN ('laki_laki','perempuan')),
    age_days     integer CHECK (age_days >= 0),
    bb_kg        numeric(6,2) NOT NULL CHECK (bb_kg > 0 AND bb_kg < 500),
    tb_cm        numeric(6,2) CHECK (tb_cm > 0 AND tb_cm < 250),
    source_ref   text NOT NULL DEFAULT 'gizi_tab' CHECK (source_ref IN ('gizi_tab','perawat_tab','vitals')),
    extras       jsonb NOT NULL DEFAULT '{}',
    zscore_bbu   numeric(5,2),
    zscore_tbu   numeric(5,2),
    zscore_bbtb  numeric(5,2),
    zscore_imtu  numeric(5,2),
    zscore_lku   numeric(5,2),
    zscore_lila  numeric(5,2),
    imt          numeric(4,1),
    imt_kategori text CHECK (imt_kategori IN ('kurus','normal','bb_lebih','obesitas')),
    status       jsonb NOT NULL DEFAULT '{}',
    curve_version text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid REFERENCES core.app_user(id),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid REFERENCES core.app_user(id),
    row_version  integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_anthropometry_person ON clinical.anthropometry (person_id, measured_at DESC);
CREATE TRIGGER trg_anthropometry_touch BEFORE UPDATE ON clinical.anthropometry
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.anthropometry ENABLE ROW LEVEL SECURITY;
CREATE POLICY anthropometry_isolation ON clinical.anthropometry
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_anthropometry_tenant_check BEFORE INSERT OR UPDATE ON clinical.anthropometry
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 4. Episode gizi (profil per admission; episode lintas admission = 2f).
CREATE TABLE clinical.nutrition_episode (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    person_id     uuid NOT NULL REFERENCES core.person(id),
    admission_id  uuid NOT NULL REFERENCES operations.admission(id),
    profile       text NOT NULL CHECK (profile IN ('dewasa','lansia','bumil','menyusui','balita')),
    risk_override text CHECK (risk_override IN ('rendah','sedang','tinggi')),
    status        text NOT NULL DEFAULT 'active' CHECK (status IN ('active','closed')),
    extras        jsonb NOT NULL DEFAULT '{}',
    started_at    timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid REFERENCES core.app_user(id),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid REFERENCES core.app_user(id),
    row_version   integer NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX uq_nutrition_episode_active ON clinical.nutrition_episode (admission_id) WHERE status = 'active';
CREATE INDEX idx_nutrition_episode_person ON clinical.nutrition_episode (person_id);
CREATE TRIGGER trg_nutrition_episode_touch BEFORE UPDATE ON clinical.nutrition_episode
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.nutrition_episode ENABLE ROW LEVEL SECURITY;
CREATE POLICY nutrition_episode_isolation ON clinical.nutrition_episode
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_nutrition_episode_tenant_check BEFORE INSERT OR UPDATE ON clinical.nutrition_episode
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 5. Audit perubahan profil episode (FR-GZ-01/15): satu baris = satu kejadian ganti + pembatalannya.
CREATE TABLE clinical.nutrition_profile_change (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    person_id    uuid NOT NULL REFERENCES core.person(id),
    episode_id   uuid NOT NULL REFERENCES clinical.nutrition_episode(id),
    from_profile text NOT NULL CHECK (from_profile IN ('dewasa','lansia','bumil','menyusui','balita')),
    to_profile   text NOT NULL CHECK (to_profile IN ('dewasa','lansia','bumil','menyusui','balita')),
    changed_at   timestamptz NOT NULL DEFAULT now(),
    changed_by   uuid REFERENCES core.app_user(id),
    reverted_at  timestamptz,
    reverted_by  uuid REFERENCES core.app_user(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    row_version  integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_npc_episode ON clinical.nutrition_profile_change (episode_id, changed_at DESC);
CREATE TRIGGER trg_npc_touch BEFORE UPDATE ON clinical.nutrition_profile_change
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.nutrition_profile_change ENABLE ROW LEVEL SECURITY;
CREATE POLICY npc_isolation ON clinical.nutrition_profile_change
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_npc_tenant_check BEFORE INSERT OR UPDATE ON clinical.nutrition_profile_change
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 6. Hasil skrining Observation-internal (teknis C; dibaca badge/Tinjau/timeline, bukan JSON form).
CREATE TABLE clinical.nutrition_screening_result (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    person_id        uuid NOT NULL REFERENCES core.person(id),
    admission_id     uuid NOT NULL REFERENCES operations.admission(id),
    episode_id       uuid REFERENCES clinical.nutrition_episode(id),
    form_instance_id uuid REFERENCES clinical_form.form_instance(id),
    instrument       text NOT NULL CHECK (instrument IN ('mst','mnasf','rules')),
    score            integer NOT NULL CHECK (score >= 0),
    max_score        integer NOT NULL CHECK (max_score > 0),
    interpretation   text NOT NULL CHECK (interpretation IN ('rendah','sedang','tinggi')),
    bands            jsonb NOT NULL DEFAULT '[]',
    payload          jsonb NOT NULL DEFAULT '{}',
    calculated_at    timestamptz NOT NULL DEFAULT now(),
    calculated_by    uuid REFERENCES core.app_user(id),
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid REFERENCES core.app_user(id),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid REFERENCES core.app_user(id),
    row_version      integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_nsr_admission ON clinical.nutrition_screening_result (admission_id, calculated_at DESC);
CREATE TRIGGER trg_nsr_touch BEFORE UPDATE ON clinical.nutrition_screening_result
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.nutrition_screening_result ENABLE ROW LEVEL SECURITY;
CREATE POLICY nsr_isolation ON clinical.nutrition_screening_result
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_nsr_tenant_check BEFORE INSERT OR UPDATE ON clinical.nutrition_screening_result
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 7. Snapshot z/IMT/status saat write (A2; ref kosong -> kolom z NULL, bukan error).
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
CREATE TRIGGER trg_anthropometry_snapshot BEFORE INSERT OR UPDATE ON clinical.anthropometry
    FOR EACH ROW EXECUTE FUNCTION clinical.fn_anthropometry_snapshot();

COMMIT;
