-- Plan 2026-11-13-gizi-nutrition-care-chunk-2-adime-core Task 1
-- (spec 2026-11-13-gizi-nutrition-care-spec §4.2): template asuhan_gizi
-- (ADIME per kunjungan) + ases_gizi_mnasf (MNA-SF, footer verbatim Nestlé).
-- Pola INSERT mengikuti 000089_clinical_form_cppt.up.sql:26-123.

INSERT INTO core.template (kind, code, name, description, scope, usage) VALUES
    ('clinical_form','asuhan_gizi','Asuhan Gizi (per kunjungan)',
     'Layar kerja gizi workspace ADIME: skrining/asesmen/PES/intervensi/monitoring per kunjungan (spec 2026-11-13-gizi)','platform','insert'),
    ('clinical_form','ases_gizi_mnasf','Skrining Gizi Lansia (MNA-SF)',
     'Mini Nutritional Assessment Short-Form; 12-14 normal, 8-11 berisiko, 0-7 malnutrisi','platform','insert');

INSERT INTO core.template_clinical_form (template_id, form_type, category, sections)
SELECT t.id, v.code AS form_type, v.category, v.sections::jsonb
FROM (VALUES
    ('asuhan_gizi','gizi','{"sections":[
        {"key":"biokimia","label":"Biokimia","kind":"repeatable","fields":[
            {"key":"item","label":"Item","type":"select","options":[
                {"value":"hb","label":"Hb (hemoglobin) g/dL"},
                {"value":"albumin","label":"Albumin serum g/dL"},
                {"value":"gdp","label":"Glukosa darah puasa mg/dL"},
                {"value":"gd2","label":"Glukosa darah 2 jam PP mg/dL"},
                {"value":"hba1c","label":"HbA1c %"}]},
            {"key":"nilai","label":"Nilai","type":"number"},
            {"key":"tanggal","label":"Tanggal","type":"date"}]},
        {"key":"diet_history","label":"Riwayat diet / diet history","kind":"textarea","soft_required":true},
        {"key":"asupan","label":"Asupan ringkas","kind":"repeatable","fields":[
            {"key":"makanan","label":"Makanan","type":"text"},
            {"key":"porsi","label":"Porsi","type":"text"},
            {"key":"frekuensi","label":"Frekuensi","type":"text"}]},
        {"key":"lingkungan","label":"Faktor lingkungan & personal","kind":"textarea"},
        {"key":"pes","label":"Diagnosis gizi (PES)","kind":"repeatable","fields":[
            {"key":"problem","label":"Problem","type":"select","options":[
                {"value":"GD-01","label":"Asupan makan kurang dari kebutuhan"},
                {"value":"GD-02","label":"Asupan makan berlebih dari kebutuhan"},
                {"value":"GD-03","label":"Perubahan berat badan tidak diinginkan"},
                {"value":"GD-04","label":"Ketidakadekuatan zat gizi makro (protein/energi)"},
                {"value":"GD-05","label":"Ketidakadekuatan zat gizi mikro"},
                {"value":"GD-06","label":"Ketidakseimbangan cairan"},
                {"value":"GD-07","label":"Kesulitan menelan/mengunyah (modifikasi tekstur)"},
                {"value":"GD-08","label":"Risiko defisit zat gizi terkait penyakit"},
                {"value":"GD-99","label":"Lainnya (jelaskan pada intervensi)"}]},
            {"key":"etiologi","label":"Etiologi","type":"text"},
            {"key":"sign","label":"Sign / symptom","type":"text"},
            {"key":"keterangan","label":"Keterangan","type":"text"}]},
        {"key":"diet_jenis","label":"Jenis diet (preskripsi)","kind":"text"},
        {"key":"diet_porsi_hari","label":"Jumlah porsi / hari","kind":"number"},
        {"key":"preskripsi","label":"Preskripsi target zat gizi","kind":"repeatable","fields":[
            {"key":"jenis","label":"Zat gizi","type":"select","options":[
                {"value":"energi_kkal","label":"Energi (kcal)"},
                {"value":"protein_g","label":"Protein (g)"},
                {"value":"lemak_g","label":"Lemak (g)"},
                {"value":"karbohidrat_g","label":"Karbohidrat (g)"},
                {"value":"cairan_ml","label":"Cairan (ml)"}]},
            {"key":"target_hari","label":"Target per hari","type":"number"},
            {"key":"target_porsi","label":"Target per porsi","type":"text"}]},
        {"key":"preskripsi_catatan","label":"Sumber perhitungan (opsional)","kind":"text"},
        {"key":"suplementasi","label":"Suplementasi","kind":"repeatable","fields":[
            {"key":"item","label":"Item","type":"text"},
            {"key":"dosis","label":"Dosis","type":"text"},
            {"key":"frekuensi","label":"Frekuensi","type":"text"}]},
        {"key":"pangan_lokal","label":"Catatan pangan lokal","kind":"textarea"},
        {"key":"edukasi","label":"Edukasi / konseling (topik terpilih)","kind":"repeatable","fields":[
            {"key":"topik","label":"Topik","type":"text"}]},
        {"key":"edukasi_catatan","label":"Catatan edukasi","kind":"textarea"},
        {"key":"me_indikator","label":"Indikator monitoring","kind":"repeatable","fields":[
            {"key":"indikator","label":"Indikator","type":"text"},
            {"key":"target","label":"Target","type":"text"}]},
        {"key":"me_rentang","label":"Rentang jadwal monitoring","kind":"text"},
        {"key":"me_frekuensi","label":"Frekuensi monitoring","kind":"select","options":[
            {"value":"harian","label":"Harian"},
            {"value":"2_4_minggu","label":"2–4 minggu"},
            {"value":"per_kunjungan","label":"Per kunjungan"}]},
        {"key":"me_reassessment","label":"Petunjuk reassessment skrining","kind":"select","options":[
            {"value":"ranap_h3","label":"rawat inap hari ke-3"},
            {"value":"ranap_h7","label":"rawat inap hari ke-7"},
            {"value":"kondisi_berubah","label":"saat kondisi berubah"},
            {"value":"saat_pulang","label":"saat pulang"}]},
        {"key":"me_evaluasi","label":"Hasil evaluasi kunjungan ini","kind":"textarea"},
        {"key":"me_status","label":"Status pencapaian","kind":"select","options":[
            {"value":"tercapai","label":"Tercapai"},
            {"value":"sebagian","label":"Sebagian"},
            {"value":"tidak","label":"Tidak"}]},
        {"key":"ttd","label":"Kepatuhan TTD/MMS harian","kind":"repeatable","fields":[
            {"key":"tanggal","label":"Tanggal","type":"date"},
            {"key":"diminum","label":"Diminum","type":"boolean"}]},
        {"key":"tinja","label":"Warna tinja (bayi 0–4 bln)","kind":"repeatable","fields":[
            {"key":"tanggal","label":"Tanggal","type":"date"},
            {"key":"nomor","label":"Nomor warna (1–7)","type":"number"}]}]}'),
    ('ases_gizi_mnasf','gizi','{"sections":[
        {"key":"mnasf_a","label":"Has food intake declined over the past 3 months due to loss of appetite, digestive problems, chewing or swallowing difficulties?","kind":"select","options":[
            {"value":"a_0","label":"0 = severe decrease in food intake"},
            {"value":"a_1","label":"1 = moderate decrease in food intake"},
            {"value":"a_2","label":"2 = no decrease in food intake"}]},
        {"key":"mnasf_b","label":"Weight loss during the last 3 months","kind":"select","options":[
            {"value":"b_0","label":"0 = weight loss greater than 3 kg (6.6 lbs)"},
            {"value":"b_1","label":"1 = does not know"},
            {"value":"b_2","label":"2 = weight loss between 1 and 3 kg (2.2 and 6.6 lbs)"},
            {"value":"b_3","label":"3 = no weight loss"}]},
        {"key":"mnasf_c","label":"Mobility","kind":"select","options":[
            {"value":"c_0","label":"0 = bed or chair bound"},
            {"value":"c_1","label":"1 = able to get out of bed / chair but does not go out"},
            {"value":"c_2","label":"2 = goes out"}]},
        {"key":"mnasf_d","label":"Has suffered psychological stress or acute disease in the past 3 months?","kind":"select","options":[
            {"value":"d_0","label":"0 = yes"},
            {"value":"d_2","label":"2 = no"}]},
        {"key":"mnasf_e","label":"Neuropsychological problems","kind":"select","options":[
            {"value":"e_0","label":"0 = severe dementia or depression"},
            {"value":"e_1","label":"1 = mild dementia"},
            {"value":"e_2","label":"2 = no psychological problems"}]},
        {"key":"mnasf_f","label":"Body Mass Index (BMI) = weight in kg / (height in m)²","kind":"select","options":[
            {"value":"f_0","label":"0 = BMI less than 19"},
            {"value":"f_1","label":"1 = BMI 19 to less than 21"},
            {"value":"f_2","label":"2 = BMI 21 to less than 23"},
            {"value":"f_3","label":"3 = BMI 23 or greater"}]},
        {"key":"skor_mnasf","label":"Skor MNA-SF total (0–14)","kind":"number"},
        {"key":"interpretasi_mnasf","label":"Interpretasi","kind":"select","options":[
            {"value":"normal","label":"12–14 Normal"},
            {"value":"berisiko","label":"8–11 Berisiko malnutrisi"},
            {"value":"malnutrisi","label":"0–7 Malnutrisi"}]},
        {"key":"atribusi","label":"Atribusi","kind":"text","value":"© Société des Produits Nestlé SA 1994, Revision 2009 — The Mini Nutritional Assessment (MNA®) and its associated logo are trademarks owned by Société des Produits Nestlé S.A."}]}')
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
    IF n <> 9 THEN
        RAISE EXCEPTION 'seed template clinical_form tidak lengkap: % dari 9', n;
    END IF;
END $$;
