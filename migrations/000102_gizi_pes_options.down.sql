-- Down 000102: restore literal sections asuhan_gizi dari 000098 (verbatim,
-- sebelum value set PES NI/NB/NC dan section diagnosis_terkait).

UPDATE core.template_clinical_form SET sections = '{"sections":[
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
            {"key":"nomor","label":"Nomor warna (1–7)","type":"number"}]}]}'::jsonb
WHERE form_type = 'asuhan_gizi';
