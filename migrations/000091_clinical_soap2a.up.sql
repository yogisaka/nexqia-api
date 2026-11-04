BEGIN;

-- CPPT Fase 2a chunk 1 (spec 2026-11-04-cppt-2a-soap-editor-design §3-§4).

-- 1. Template soap_dokter v2: hanya MENAMBAH section key (draft v1 tetap
--    valid = subset key); versi dipin per instance saat create.
UPDATE core.template_clinical_form f
SET sections = v.sections::jsonb
FROM core.template t, (VALUES
    ('soap_dokter', '{"sections":[
        {"key":"keluhan_utama","label":"Keluhan utama","kind":"text","soft_required":true},
        {"key":"subjective","label":"Anamnesa","kind":"textarea","soft_required":true},
        {"key":"alergi","label":"Alergi","kind":"repeatable","soft_required":true,"fields":[
            {"key":"pemicu","label":"Pemicu","type":"text"},
            {"key":"jenis","label":"Jenis","type":"select","options":[
                {"value":"obat","label":"Obat"},
                {"value":"makanan","label":"Makanan"},
                {"value":"lingkungan","label":"Lingkungan"},
                {"value":"lainnya","label":"Lainnya"}]},
            {"key":"reaksi","label":"Reaksi","type":"text"},
            {"key":"keparahan","label":"Keparahan","type":"select","options":[
                {"value":"ringan","label":"Ringan"},
                {"value":"sedang","label":"Sedang"},
                {"value":"berat","label":"Berat"}]},
            {"key":"efek_samping","label":"Efek samping","type":"text"},
            {"key":"tanggal","label":"Tanggal kejadian","type":"date"}]},
        {"key":"objective","label":"O — Objektif (tanda vital)","kind":"vitals"},
        {"key":"gcs_eye","label":"Pembukaan mata (E 1-4)","kind":"select","options":[
            {"value":"4","label":"4 — spontan"},
            {"value":"3","label":"3 — terhadap suara"},
            {"value":"2","label":"2 — terhadap rangsang nyeri"},
            {"value":"1","label":"1 — tidak ada"}]},
        {"key":"gcs_verbal","label":"Respons verbal (V 1-5)","kind":"select","options":[
            {"value":"5","label":"5 — orientasi baik"},
            {"value":"4","label":"4 — bicara bingung"},
            {"value":"3","label":"3 — kata-kata tidak tepat"},
            {"value":"2","label":"2 — suara tanpa arti"},
            {"value":"1","label":"1 — tidak ada respons"},
            {"value":"t","label":"T — tidak dapat dinilai"}]},
        {"key":"gcs_motorik","label":"Respons motorik (M 1-6)","kind":"select","options":[
            {"value":"6","label":"6 — mengikuti perintah"},
            {"value":"5","label":"5 — melokalisasi nyeri"},
            {"value":"4","label":"4 — menarik anggota tubuh"},
            {"value":"3","label":"3 — fleksi abnormal"},
            {"value":"2","label":"2 — ekstensi abnormal"},
            {"value":"1","label":"1 — tidak ada respons"},
            {"value":"t","label":"T — tidak dapat dinilai"}]},
        {"key":"gcs_total","label":"Total GCS","kind":"calc","calc":{"formula":"gcs","inputs":["gcs_eye","gcs_verbal","gcs_motorik"]}},
        {"key":"imt","label":"IMT","kind":"calc","calc":{"formula":"imt","inputs":["objective.weight_kg","objective.height_cm"]}},
        {"key":"assessment","label":"A — Asesmen (diagnosis)","kind":"diagnosis","soft_required":true},
        {"key":"plan","label":"P — Rencana (tindakan / instruksi)","kind":"textarea","soft_required":true},
        {"key":"rekomendasi","label":"Rekomendasi follow-up","kind":"text","soft_required":true}]}'::text)
) AS v(code, sections)
WHERE t.id = f.template_id
  AND t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = v.code;

UPDATE core.template SET version = 2
WHERE kind = 'clinical_form' AND scope = 'platform' AND code = 'soap_dokter';

DO $$ BEGIN
    IF (SELECT count(*) FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter') <> 1
       OR (SELECT t.version FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter') <> 2 THEN
        RAISE EXCEPTION 'seed soap_dokter v2 gagal';
    END IF;
END $$;

-- 2. Template konten pribadi (spec §1 D3): snapshot form_data milik satu
--    user ("Jadikan template") — bukan core.template (struktur vs konten;
--    template_clinical_form write di-revoke dari app_runtime, 000089).
CREATE TABLE clinical_form.content_template (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    owner_user_id uuid NOT NULL REFERENCES core.app_user(id),
    form_type     text NOT NULL,
    name          text NOT NULL,
    form_data     jsonb NOT NULL,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT uq_content_template_name UNIQUE (owner_user_id, form_type, name)
);
CREATE INDEX idx_content_template_owner ON clinical_form.content_template (owner_user_id, form_type);
CREATE TRIGGER trg_content_template_touch BEFORE UPDATE ON clinical_form.content_template
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE clinical_form.content_template ENABLE ROW LEVEL SECURITY;
CREATE POLICY content_template_isolation ON clinical_form.content_template
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_content_template_tenant_check BEFORE INSERT OR UPDATE ON clinical_form.content_template
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

COMMIT;
