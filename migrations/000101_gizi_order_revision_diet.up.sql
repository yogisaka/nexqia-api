CREATE TABLE clinical.nutrition_order_revision (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    order_id     uuid NOT NULL REFERENCES clinical.nutrition_order(id) ON DELETE CASCADE,
    row_version  integer NOT NULL,
    action       text NOT NULL CHECK (action IN ('create','revise','stop')),
    note         text,
    changed_by   uuid NOT NULL REFERENCES core.app_user(id),
    changed_at   timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_nutrition_order_revision_order ON clinical.nutrition_order_revision (order_id, changed_at DESC);
ALTER TABLE clinical.nutrition_order_revision ENABLE ROW LEVEL SECURITY;
CREATE POLICY nutrition_order_revision_isolation ON clinical.nutrition_order_revision
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_nutrition_order_revision_tenant_check BEFORE INSERT ON clinical.nutrition_order_revision
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

INSERT INTO core.diet_type (company_id, merchant_id, code, name, default_calorie, created_by)
SELECT m.company_id, m.id, v.code, v.name, v.kcal, NULL
FROM core.merchant m
CROSS JOIN (VALUES
    ('DIET-BIASA','Diet Biasa',NULL::numeric),
    ('DIET-LUNAK','Diet Lunak',NULL::numeric),
    ('DIET-LUNAK-SARING','Diet Lunak Saring',NULL::numeric),
    ('DIET-CAIR-JERNIH','Diet Cair Jernih',NULL::numeric),
    ('DIET-CAIR-PENUH','Diet Cair Penuh',NULL::numeric),
    ('DIET-CAIR-KENTAL','Diet Cair Kental',NULL::numeric),
    ('DIET-SARING','Diet Saring',NULL::numeric),
    ('DIET-DM','Diet Diabetes Mellitus',1700::numeric),
    ('DIET-RG','Diet Rendah Garam',NULL::numeric),
    ('DIET-RL','Diet Rendah Lemak',NULL::numeric),
    ('DIET-RP','Diet Rendah Protein',NULL::numeric),
    ('DIET-PANTANG','Diet Pantang Sementara',NULL::numeric)
) AS v(code, name, kcal)
ON CONFLICT (merchant_id, code) DO NOTHING;

-- D26 amandemen (user 2026-11-15): opsi MST penurunan_bb 0-3 (skala total 0-5,
-- konsisten label template "Skor MST total (0-5)" 000089:74 + test skor 5 + mockup Ferguson)
UPDATE core.template_clinical_form
SET sections = '{"sections":[
    {"key":"mst_penurunan_bb","label":"MST 1 — Penurunan berat badan (skor)","kind":"select","options":[
        {"value":"0","label":"Tidak ada penurunan"},
        {"value":"1","label":"Turun 1-3 kg (3 bulan)"},
        {"value":"2","label":"Turun >3 kg atau tidak diketahui"},
        {"value":"3","label":"Turun ±10 kg (6 bulan)"}]},
    {"key":"mst_nafsu_makan","label":"MST 2 — Nafsu makan berkurang karena sakit (skor)","kind":"select","options":[
        {"value":"0","label":"Tidak"},
        {"value":"1","label":"Ya"}]},
    {"key":"mst_sakit_akut","label":"MST 3 — Sakit akut / rawat inap 5 hari terakhir (skor)","kind":"select","options":[
        {"value":"0","label":"Tidak"},
        {"value":"1","label":"Ya"}]},
    {"key":"skor_mst","label":"Skor MST total (0-5)","kind":"number"},
    {"key":"risiko_gizi","label":"Status risiko gizi","kind":"select","options":[
        {"value":"tidak_berisiko","label":"Tidak berisiko (skor < 2)"},
        {"value":"berisiko","label":"Berisiko malnutrisi (skor >= 2)"}]}]}'::jsonb
WHERE form_type = 'ases_gizi_skrining';
