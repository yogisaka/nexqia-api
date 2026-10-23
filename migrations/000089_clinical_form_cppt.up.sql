BEGIN;

-- CPPT Fase 1 (spec 2026-10-21-cppt-rajal-fase1-design §4).

-- 1. Template clinical_form (usage='insert').
ALTER TABLE core.template DROP CONSTRAINT template_kind_check;
ALTER TABLE core.template ADD CONSTRAINT template_kind_check
    CHECK (kind IN ('role', 'department', 'price_list', 'clinical_form'));
ALTER TABLE core.template ADD CONSTRAINT chk_template_clinical_form_usage
    CHECK (kind <> 'clinical_form' OR usage = 'insert');

CREATE TABLE core.template_clinical_form (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    template_id  uuid NOT NULL REFERENCES core.template(id) ON DELETE CASCADE,
    form_type    text NOT NULL,
    category     text NOT NULL CHECK (category IN
                 ('soap','keperawatan','assessment','gizi','resume','handover')),
    sections     jsonb NOT NULL,
    UNIQUE (template_id, form_type)
);
ALTER TABLE core.template_clinical_form ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_clinical_form_visibility ON core.template_clinical_form
    USING (EXISTS (SELECT 1 FROM core.template t WHERE t.id = template_id));
REVOKE INSERT, UPDATE, DELETE ON core.template_clinical_form FROM app_runtime;

INSERT INTO core.template (kind, code, name, description, scope, usage) VALUES
    ('clinical_form','soap_dokter','SOAP Dokter (Rawat Jalan)',
     'Catatan perkembangan dokter rawat jalan: Subjektif-Objektif-Asesmen-Rencana','platform','insert'),
    ('clinical_form','ases_awal_perawat','Asesmen Awal Perawat (Rawat Jalan)',
     'Anamnesis, konfirmasi alergi, tanda vital, nyeri, risiko jatuh (manual)','platform','insert'),
    ('clinical_form','ases_gizi_skrining','Skrining Gizi (MST)',
     'Malnutrition Screening Tool; skor >= 2 = berisiko malnutrisi','platform','insert'),
    ('clinical_form','ases_gizi_adime','Asesmen Gizi ADIME',
     'Assessment, Diagnosis gizi, Intervention, Monitoring/Evaluation (PAGT)','platform','insert'),
    ('clinical_form','monitoring_gizi','Monitoring & Evaluasi Gizi',
     'Catatan monitoring/evaluasi asuhan gizi per kunjungan rawat jalan','platform','insert'),
    ('clinical_form','resume_rajal','Resume Medis Rawat Jalan',
     'Resume & discharge simple rawat jalan (kondisi pulang, instruksi, kontrol)','platform','insert'),
    ('clinical_form','handover_sbar','Handover SBAR',
     'Fondasi form serah terima SBAR (workflow handover penuh = Fase 2)','platform','insert');

INSERT INTO core.template_clinical_form (template_id, form_type, category, sections)
SELECT t.id, v.code AS form_type, v.category, v.sections::jsonb
FROM (VALUES
    ('soap_dokter','soap','{"sections":[
        {"key":"subjective","label":"S — Subjektif","kind":"textarea","soft_required":true},
        {"key":"objective","label":"O — Objektif (tanda vital)","kind":"vitals"},
        {"key":"assessment","label":"A — Asesmen (diagnosis)","kind":"diagnosis","soft_required":true},
        {"key":"plan","label":"P — Rencana","kind":"textarea","soft_required":true}]}'),
    ('ases_awal_perawat','keperawatan','{"sections":[
        {"key":"keluhan","label":"Keluhan utama","kind":"textarea","soft_required":true},
        {"key":"riwayat_penyakit","label":"Riwayat penyakit sekarang & dahulu","kind":"textarea"},
        {"key":"alergi_konfirmasi","label":"Konfirmasi alergi","kind":"select","options":[
            {"value":"tidak_ada","label":"Tidak ada"},
            {"value":"ada_sesuai_riwayat","label":"Ada — sesuai riwayat tercatat"},
            {"value":"ada_baru","label":"Ada — baru ditemukan"},
            {"value":"tidak_diketahui","label":"Tidak diketahui"}]},
        {"key":"tanda_vital","label":"Tanda vital","kind":"vitals"},
        {"key":"nyeri","label":"Skor nyeri (0-10)","kind":"number"},
        {"key":"risiko_jatuh","label":"Risiko jatuh (penilaian manual)","kind":"select","options":[
            {"value":"tidak_berisiko","label":"Tidak berisiko"},
            {"value":"berisiko","label":"Berisiko"}]}]}'),
    ('ases_gizi_skrining','assessment','{"sections":[
        {"key":"mst_penurunan_bb","label":"MST 1 — Penurunan berat badan (skor)","kind":"select","options":[
            {"value":"0","label":"Tidak ada penurunan"},
            {"value":"1","label":"Turun 1-3 kg (3 bulan)"},
            {"value":"2","label":"Turun >3 kg atau tidak diketahui"}]},
        {"key":"mst_nafsu_makan","label":"MST 2 — Nafsu makan berkurang karena sakit (skor)","kind":"select","options":[
            {"value":"0","label":"Tidak"},
            {"value":"1","label":"Ya"}]},
        {"key":"mst_sakit_akut","label":"MST 3 — Sakit akut / rawat inap 5 hari terakhir (skor)","kind":"select","options":[
            {"value":"0","label":"Tidak"},
            {"value":"1","label":"Ya"}]},
        {"key":"skor_mst","label":"Skor MST total (0-5)","kind":"number"},
        {"key":"risiko_gizi","label":"Status risiko gizi","kind":"select","options":[
            {"value":"tidak_berisiko","label":"Tidak berisiko (skor < 2)"},
            {"value":"berisiko","label":"Berisiko malnutrisi (skor >= 2)"}]}]}'),
    ('ases_gizi_adime','gizi','{"sections":[
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
        {"key":"monitoring_evaluation","label":"M/E — Rencana monitoring & evaluasi","kind":"textarea"}]}'),
    ('monitoring_gizi','gizi','{"sections":[
        {"key":"hasil_monitoring","label":"Hasil monitoring (antropometri/asupan)","kind":"textarea","soft_required":true},
        {"key":"evaluasi","label":"Evaluasi pencapaian","kind":"textarea"},
        {"key":"tindak_lanjut","label":"Tindak lanjut","kind":"textarea"}]}'),
    ('resume_rajal','resume','{"sections":[
        {"key":"kondisi_pulang","label":"Kondisi saat pulang","kind":"select","options":[
            {"value":"stabil","label":"Stabil"},
            {"value":"tidak_stabil","label":"Tidak stabil"},
            {"value":"perbaikan","label":"Perbaikan"}]},
        {"key":"instruksi","label":"Instruksi & edukasi pulang","kind":"textarea","soft_required":true},
        {"key":"kontrol_tanggal","label":"Tanggal kontrol","kind":"date"},
        {"key":"catatan","label":"Catatan","kind":"textarea"}]}'),
    ('handover_sbar','handover','{"sections":[
        {"key":"situation","label":"S — Situation","kind":"textarea"},
        {"key":"background","label":"B — Background","kind":"textarea"},
        {"key":"assessment","label":"A — Assessment","kind":"textarea"},
        {"key":"recommendation","label":"R — Recommendation","kind":"textarea"}]}')
) AS v(code, category, sections)
JOIN core.template t ON t.code = v.code AND t.scope = 'platform' AND t.kind = 'clinical_form'
ON CONFLICT DO NOTHING;

DO $$
DECLARE n integer;
BEGIN
    SELECT count(*) INTO n
    FROM core.template t
    JOIN core.template_clinical_form f ON f.template_id = t.id
    WHERE t.scope = 'platform' AND t.kind = 'clinical_form' AND t.is_active;
    IF n <> 7 THEN
        RAISE EXCEPTION 'seed template clinical_form tidak lengkap: % dari 7', n;
    END IF;
END $$;

-- 2. Entri catatan klinis (spec §4.2).
CREATE SCHEMA IF NOT EXISTS clinical_form;
GRANT USAGE ON SCHEMA clinical_form TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA clinical_form GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA clinical_form GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;

CREATE TABLE clinical_form.form_instance (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    admission_id     uuid NOT NULL REFERENCES operations.admission(id),
    form_type        text NOT NULL,
    template_id      uuid NOT NULL REFERENCES core.template(id),
    template_version integer NOT NULL,
    form_data        jsonb NOT NULL,
    status           text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','signed')),
    filled_by        uuid NOT NULL REFERENCES core.app_user(id),
    filled_at        timestamptz NOT NULL DEFAULT now(),
    signed_by        uuid REFERENCES core.app_user(id),
    signed_at        timestamptz,
    signature_hash   text,
    counter_signed_by uuid REFERENCES core.app_user(id),
    counter_signed_at timestamptz,
    addendum_to_id   uuid REFERENCES clinical_form.form_instance(id),
    addendum_reason  text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    deleted_at       timestamptz,
    deleted_by       uuid REFERENCES core.app_user(id),
    row_version      integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_form_instance_signed CHECK (
        (status = 'draft' AND signed_by IS NULL AND signed_at IS NULL AND signature_hash IS NULL)
        OR (status = 'signed' AND signed_by IS NOT NULL AND signed_at IS NOT NULL AND signature_hash IS NOT NULL)),
    CONSTRAINT chk_form_instance_addendum_reason CHECK (
        addendum_to_id IS NULL OR addendum_reason IS NOT NULL),
    CONSTRAINT chk_form_instance_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE INDEX idx_form_instance_admission ON clinical_form.form_instance (admission_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_form_instance_addendum ON clinical_form.form_instance (addendum_to_id) WHERE addendum_to_id IS NOT NULL;
CREATE INDEX idx_form_instance_data_gin ON clinical_form.form_instance USING gin (form_data);
CREATE TRIGGER trg_form_instance_touch BEFORE UPDATE ON clinical_form.form_instance
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical_form.form_instance ENABLE ROW LEVEL SECURITY;
CREATE POLICY form_instance_isolation ON clinical_form.form_instance
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_form_instance_tenant_check BEFORE INSERT OR UPDATE ON clinical_form.form_instance
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 3. Diagnosis kunjungan kode ganda (spec §4.4).
CREATE SCHEMA IF NOT EXISTS clinical;
GRANT USAGE ON SCHEMA clinical TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA clinical GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA clinical GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;

CREATE TABLE clinical.diagnosis (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    admission_id      uuid NOT NULL REFERENCES operations.admission(id),
    form_instance_id  uuid REFERENCES clinical_form.form_instance(id),
    dx_role           text NOT NULL CHECK (dx_role IN ('primary','secondary')),
    concept_id        uuid NOT NULL REFERENCES terminology.concept(id),
    primary_system    text NOT NULL,
    primary_code      text NOT NULL,
    primary_display   text NOT NULL,
    icd10_concept_id  uuid REFERENCES terminology.concept(id),
    icd10_code        text,
    icd10_display     text,
    icd10_source      text CHECK (icd10_source IS NULL OR icd10_source IN ('auto','manual')),
    icd10_map_set     text,
    icd10_chosen_by   uuid REFERENCES core.app_user(id),
    icd10_chosen_at   timestamptz,
    created_at        timestamptz NOT NULL DEFAULT now(),
    created_by        uuid NOT NULL REFERENCES core.app_user(id),
    row_version       integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_diagnosis_admission ON clinical.diagnosis (admission_id);
CREATE INDEX idx_diagnosis_concept ON clinical.diagnosis (concept_id);
CREATE TRIGGER trg_diagnosis_touch BEFORE UPDATE ON clinical.diagnosis
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.diagnosis ENABLE ROW LEVEL SECURITY;
CREATE POLICY diagnosis_isolation ON clinical.diagnosis
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_diagnosis_tenant_check BEFORE INSERT OR UPDATE ON clinical.diagnosis
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 4. Order diet (docs/10 §2b + deviasi spec §4.3: physician nullable, ordered_by dietisien).
CREATE TABLE clinical.nutrition_order (
    id                    uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id            uuid NOT NULL REFERENCES core.company(id),
    merchant_id           uuid NOT NULL REFERENCES core.merchant(id),
    admission_id          uuid NOT NULL REFERENCES operations.admission(id),
    ordering_physician_id uuid REFERENCES core.physician(id),
    ordered_by            uuid NOT NULL REFERENCES core.app_user(id),
    diet_type_id          uuid NOT NULL REFERENCES core.diet_type(id),
    texture               text CHECK (texture IN ('biasa','lunak','lunak_saring','cair','pantang')),
    route                 text NOT NULL DEFAULT 'oral' CHECK (route IN ('oral','ngt','parenteral')),
    calorie_target        numeric(6,2),
    restriction_note      text,
    status                text NOT NULL DEFAULT 'active' CHECK (status IN ('active','completed','cancelled')),
    started_at            timestamptz NOT NULL DEFAULT now(),
    ended_at              timestamptz,
    created_at            timestamptz NOT NULL DEFAULT now(),
    created_by            uuid REFERENCES core.app_user(id),
    updated_at            timestamptz NOT NULL DEFAULT now(),
    updated_by            uuid REFERENCES core.app_user(id),
    row_version           integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_nutrition_order_admission ON clinical.nutrition_order (admission_id);
CREATE TRIGGER trg_nutrition_order_touch BEFORE UPDATE ON clinical.nutrition_order
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical.nutrition_order ENABLE ROW LEVEL SECURITY;
CREATE POLICY nutrition_order_isolation ON clinical.nutrition_order
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_nutrition_order_tenant_check BEFORE INSERT OR UPDATE ON clinical.nutrition_order
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- 5. Izin + role Dietisien + versi template role (pola 000043 + 000078).
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000050', 'clinical.record.write',
     'Membuat dan menandatangani entri catatan klinis (CPPT)', 'clinical'),
    ('00000000-0000-0000-0004-000000000051', 'clinical.record.countersign',
     'Counter-sign DPJP untuk entri klinis profesi lain', 'clinical'),
    ('00000000-0000-0000-0004-000000000052', 'clinical.nutrition.write',
     'Asuhan gizi: asesmen gizi, order diet, katalog diet', 'clinical'),
    ('00000000-0000-0000-0004-000000000053', 'terminology.lookup',
     'Lookup terminology kode', 'terminology')
ON CONFLICT DO NOTHING;

INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN
    ('clinical.record.write', 'clinical.record.countersign', 'clinical.nutrition.write')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role (template_id, name, description, sort_order)
SELECT t.id, 'Dietisien',
       'Asuhan gizi terstandar (PAGT): skrining, asesmen ADIME, order diet.', 90
FROM core.template t
WHERE t.scope = 'platform' AND t.kind = 'role'
  AND t.code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit')
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('clinical.record.view', 'clinical.nutrition.write')
WHERE tr.name = 'Dietisien'
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'clinical.record.write'
WHERE tr.name IN ('Dokter', 'Dokter Spesialis', 'Perawat')
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'clinical.record.countersign'
WHERE tr.name IN ('Dokter', 'Dokter Spesialis')
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'terminology.lookup'
WHERE tr.name IN ('Dietisien', 'Dokter', 'Dokter Spesialis', 'Perawat')
ON CONFLICT DO NOTHING;

UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

COMMIT;
