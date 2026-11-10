BEGIN;

-- Rollback soap_dokter v3 -> v2. Draft yang dihapus up TIDAK dipulihkan.

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
    IF (SELECT t.version FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter') <> 2 THEN
        RAISE EXCEPTION 'rollback soap_dokter v2 gagal';
    END IF;
END $$;

COMMIT;
