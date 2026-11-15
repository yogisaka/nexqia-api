DROP TABLE IF EXISTS clinical.nutrition_order_revision;
DELETE FROM core.diet_type WHERE code IN ('DIET-BIASA','DIET-LUNAK','DIET-LUNAK-SARING','DIET-CAIR-JERNIH','DIET-CAIR-PENUH','DIET-CAIR-KENTAL','DIET-SARING','DIET-DM','DIET-RL','DIET-RP','DIET-PANTANG');
UPDATE core.template_clinical_form
SET sections = '{"sections":[
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
        {"value":"berisiko","label":"Berisiko malnutrisi (skor >= 2)"}]}]}'::jsonb
WHERE form_type = 'ases_gizi_skrining';
