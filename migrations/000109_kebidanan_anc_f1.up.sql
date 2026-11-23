BEGIN;

-- 1. Episode perawatan generik (blueprint #25; F1: kind kehamilan).
CREATE TABLE operations.care_episode (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    person_id     uuid NOT NULL REFERENCES core.person(id),
    kind          text NOT NULL CHECK (kind IN ('kehamilan')),
    ordinal_no    integer NOT NULL CHECK (ordinal_no > 0),
    status        text NOT NULL CHECK (status IN ('active','closed')),
    started_at    timestamptz NOT NULL DEFAULT now(),
    closed_at     timestamptz,
    close_context jsonb,
    row_version   integer NOT NULL DEFAULT 0,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now()
);
CREATE UNIQUE INDEX uq_care_episode_active ON operations.care_episode (person_id, kind) WHERE status = 'active';
CREATE TRIGGER trg_care_episode_touch BEFORE UPDATE ON operations.care_episode
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.care_episode ENABLE ROW LEVEL SECURITY;
CREATE POLICY care_episode_isolation ON operations.care_episode
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_care_episode_tenant_check BEFORE INSERT OR UPDATE ON operations.care_episode
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 2. Detail kehamilan (1:1 episode).
CREATE TABLE clinical.midwifery_pregnancy (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    care_episode_id uuid NOT NULL UNIQUE REFERENCES operations.care_episode(id),
    gravida         integer NOT NULL,
    para            integer NOT NULL DEFAULT 0,
    abortus         integer NOT NULL DEFAULT 0,
    riwayat_persalinan text,
    jarak_kehamilan_th integer,
    hpht            date NOT NULL,
    uk_source       text NOT NULL DEFAULT 'hpht' CHECK (uk_source IN ('hpht','usg_t1')),
    uk_usg_t1_weeks numeric(4,1),
    hpl             date NOT NULL,
    row_version     integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_midwifery_pregnancy_touch BEFORE UPDATE ON clinical.midwifery_pregnancy
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.midwifery_pregnancy ENABLE ROW LEVEL SECURITY;
CREATE POLICY midwifery_pregnancy_isolation ON clinical.midwifery_pregnancy
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_midwifery_pregnancy_tenant_check BEFORE INSERT OR UPDATE ON clinical.midwifery_pregnancy
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 3. Kunjungan ANC (topik 2/4/5 + imunisasi lainnya).
CREATE TABLE clinical.midwifery_anc_visit (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    care_episode_id   uuid NOT NULL REFERENCES operations.care_episode(id),
    admission_id      uuid NOT NULL REFERENCES operations.admission(id),
    visited_at        timestamptz NOT NULL DEFAULT now(),
    tfu_cm            numeric(4,1),
    djj               integer CHECK (djj IS NULL OR djj BETWEEN 60 AND 220),
    presentation      text CHECK (presentation IN ('kepala','sungsang','melintang','belumdapat')),
    placenta_location text CHECK (placenta_location IN ('fundus','corpus','letak_rendah','previa','belumdapat')),
    inspekulo         jsonb,
    tanda_bahaya      jsonb,
    jiwa              jsonb,
    imunisasi_lainnya text,
    next_visit_date   date,
    row_version       integer NOT NULL DEFAULT 0,
    created_at        timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_midwifery_anc_visit_episode ON clinical.midwifery_anc_visit (care_episode_id, visited_at DESC);
CREATE TRIGGER trg_midwifery_anc_visit_touch BEFORE UPDATE ON clinical.midwifery_anc_visit
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.midwifery_anc_visit ENABLE ROW LEVEL SECURITY;
CREATE POLICY midwifery_anc_visit_isolation ON clinical.midwifery_anc_visit
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_midwifery_anc_visit_tenant_check BEFORE INSERT OR UPDATE ON clinical.midwifery_anc_visit
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 4. Skrining preeklampsia (episode-level; boleh >1 baris, terakhir menang).
CREATE TABLE clinical.midwifery_pe_screen (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    care_episode_id uuid NOT NULL REFERENCES operations.care_episode(id),
    screened_at     timestamptz NOT NULL DEFAULT now(),
    factors         jsonb NOT NULL,
    band            text NOT NULL CHECK (band IN ('rendah','sedang','tinggi')),
    rujukan_flag    boolean NOT NULL DEFAULT false,
    row_version     integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE TRIGGER trg_midwifery_pe_screen_touch BEFORE UPDATE ON clinical.midwifery_pe_screen
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.midwifery_pe_screen ENABLE ROW LEVEL SECURITY;
CREATE POLICY midwifery_pe_screen_isolation ON clinical.midwifery_pe_screen
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_midwifery_pe_screen_tenant_check BEFORE INSERT OR UPDATE ON clinical.midwifery_pe_screen
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
-- KONTRAK jsonb factors literal: {"primigravida":false,"jarak_gt10th":false,"riwayat_pe_keluarga":true,"imt_gt30":false,"multipel":false}

-- 5. USG biometri per trimester.
CREATE TABLE clinical.midwifery_usg (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    care_episode_id uuid NOT NULL REFERENCES operations.care_episode(id),
    trimester       text NOT NULL CHECK (trimester IN ('t1','t3')),
    usg_date        date NOT NULL,
    crl_mm          numeric(5,1),
    gs_mm           numeric(5,1),
    bpd_mm          numeric(5,1),
    hc_mm           numeric(5,1),
    ac_mm           numeric(5,1),
    fl_mm           numeric(5,1),
    efw_g           integer,
    sdp             text,
    row_version     integer NOT NULL DEFAULT 0,
    created_at      timestamptz NOT NULL DEFAULT now(),
    updated_at      timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_midwifery_usg_episode ON clinical.midwifery_usg (care_episode_id, usg_date DESC);
CREATE TRIGGER trg_midwifery_usg_touch BEFORE UPDATE ON clinical.midwifery_usg
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.midwifery_usg ENABLE ROW LEVEL SECURITY;
CREATE POLICY midwifery_usg_isolation ON clinical.midwifery_usg
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_midwifery_usg_tenant_check BEFORE INSERT OR UPDATE ON clinical.midwifery_usg
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 6. Jadwal imunisasi (ref global tanpa tenant, pola ref.growth_curve 000097:9-24).
CREATE TABLE ref.immunization_schedule (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    antigen_slug       text NOT NULL,
    label              text NOT NULL,
    dosis_ke           integer NOT NULL CHECK (dosis_ke > 0),
    kategori           text NOT NULL CHECK (kategori IN ('dasar','lanjutan','bias','ibu_hamil')),
    usia_min_bln       integer,
    usia_maks_bln      integer,
    interval_hari_dari_dosis_sebelumnya integer,
    masa_perlindungan_tahun integer,
    source_name        text NOT NULL,
    source_version     text NOT NULL,
    source_url         text NOT NULL,
    downloaded_at      timestamptz NOT NULL,
    UNIQUE (antigen_slug, dosis_ke, kategori, source_version)
);
GRANT SELECT ON ref.immunization_schedule TO app_runtime;

-- 7. Riwayat vaksin person (tabel BERSAMA anak A2 — jangan duplikat konsep nanti).
CREATE TABLE clinical.person_riwayat_vaksin (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    merchant_id        uuid NOT NULL REFERENCES core.merchant(id),
    person_id          uuid NOT NULL REFERENCES core.person(id),
    antigen_slug       text NOT NULL,
    dosis_ke           integer NOT NULL CHECK (dosis_ke > 0),
    given_date         date NOT NULL,
    no_batch           text,
    tempat             text CHECK (tempat IN ('faskes_ini','luar')),
    tempat_keterangan  text,
    pemberi            text,
    row_version        integer NOT NULL DEFAULT 0,
    created_at         timestamptz NOT NULL DEFAULT now(),
    updated_at         timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_person_riwayat_vaksin_person ON clinical.person_riwayat_vaksin (person_id, antigen_slug, given_date DESC);
CREATE TRIGGER trg_person_riwayat_vaksin_touch BEFORE UPDATE ON clinical.person_riwayat_vaksin
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.person_riwayat_vaksin ENABLE ROW LEVEL SECURITY;
CREATE POLICY person_riwayat_vaksin_isolation ON clinical.person_riwayat_vaksin
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_person_riwayat_vaksin_tenant_check BEFORE INSERT OR UPDATE ON clinical.person_riwayat_vaksin
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 8. Seed jadwal Td (verbatim Buku KIA 2024 hlm ±38; interval konversi 1 bln=30 hr, 6 bln=180 hr, 12 bln=365 hr).
INSERT INTO ref.immunization_schedule
    (antigen_slug, label, dosis_ke, kategori, interval_hari_dari_dosis_sebelumnya, masa_perlindungan_tahun,
     source_name, source_version, source_url, downloaded_at)
VALUES
    ('td', 'Td (Tetanus-Difteri)', 1, 'ibu_hamil', NULL, NULL, 'Buku KIA 2024', '2024', 'docs/agent/ref/cppt/gizi/bukukia-2024.txt:4171-4184', now()),
    ('td', 'Td (Tetanus-Difteri)', 2, 'ibu_hamil', 30, 3, 'Buku KIA 2024', '2024', 'docs/agent/ref/cppt/gizi/bukukia-2024.txt:4171-4184', now()),
    ('td', 'Td (Tetanus-Difteri)', 3, 'ibu_hamil', 180, 5, 'Buku KIA 2024', '2024', 'docs/agent/ref/cppt/gizi/bukukia-2024.txt:4171-4184', now()),
    ('td', 'Td (Tetanus-Difteri)', 4, 'ibu_hamil', 365, 10, 'Buku KIA 2024', '2024', 'docs/agent/ref/cppt/gizi/bukukia-2024.txt:4171-4184', now()),
    ('td', 'Td (Tetanus-Difteri)', 5, 'ibu_hamil', 365, 25, 'Buku KIA 2024', '2024', 'docs/agent/ref/cppt/gizi/bukukia-2024.txt:4171-4184', now())
ON CONFLICT (antigen_slug, dosis_ke, kategori, source_version) DO NOTHING;

-- 9. Permission + role binding (POLA 000089:246-277 — permission di MIGRATION, bukan seed).
-- UUID 00000000-0000-0000-0004-000000000054: pra-cek migrations, nomor tertinggi terpakai = 053 (000089).
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000054', 'clinical.midwifery.write',
     'Asuhan kebidanan: episode kehamilan, ANC, imunisasi Td ibu hamil', 'clinical')
ON CONFLICT DO NOTHING;

INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('clinical.midwifery.write')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role (template_id, name, description, sort_order)
SELECT t.id, 'Bidan',
       'Asuhan kebidanan terstandar: episode kehamilan, ANC, imunisasi Td ibu hamil.', 91
FROM core.template t
WHERE t.scope = 'platform' AND t.kind = 'role'
  AND t.code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit')
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('clinical.record.view', 'clinical.midwifery.write')
WHERE tr.name = 'Bidan'
ON CONFLICT DO NOTHING;

COMMIT;
