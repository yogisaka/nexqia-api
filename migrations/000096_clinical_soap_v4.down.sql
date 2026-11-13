BEGIN;

-- Rollback soap_dokter v4 -> v3. Draft yang dihapus up TIDAK dipulihkan;
-- baris alergi dengan `waktu` tetap tersimpan (field tak dikenal hanya
-- ditolak saat tulis baru, tidak memengaruhi data eksisting).

UPDATE core.template_clinical_form f
SET sections = v.sections::jsonb
FROM core.template t, (VALUES
    ('soap_dokter', '{"sections":[
        {"key":"anamnesis_info","label":"Informasi Anamnesis","kind":"radio_conditional","options":[
            {"value":"autoanamnesis","label":"Autoanamnesis"},
            {"value":"alloanamnesis","label":"Alloanamnesis"}],
         "meta":{
            "hubungan":[{"value":"orang_tua","label":"Orang tua"},{"value":"suami_istri","label":"Suami/istri"},{"value":"anak","label":"Anak"},{"value":"saudara","label":"Saudara"},{"value":"lainnya","label":"Lainnya"}],
            "keandalan":[{"value":"andal","label":"Andal"},{"value":"kurang","label":"Kurang andal"},{"value":"tidak_dapat_dinilai","label":"Tidak dapat dinilai"}]}},
        {"key":"keluhan_utama","label":"Keluhan utama","kind":"text","soft_required":true},
        {"key":"rps","label":"Riwayat penyakit sekarang","kind":"textarea"},
        {"key":"rpd","label":"Riwayat penyakit dahulu","kind":"textarea"},
        {"key":"obat_rutin","label":"Riwayat obat rutin","kind":"textarea"},
        {"key":"riwayat_keluarga","label":"Riwayat keluarga","kind":"textarea"},
        {"key":"riwayat_sosial","label":"Riwayat sosial","kind":"textarea"},
        {"key":"psikososial_spiritual","label":"Psikososial & spiritual","kind":"textarea"},
        {"key":"alergi","label":"Alergi","kind":"repeatable","soft_required":true,"fields":[
            {"key":"pemicu","label":"Pemicu","type":"text"},
            {"key":"jenis","label":"Jenis","type":"select","options":[
                {"value":"drug","label":"Obat"},{"value":"food","label":"Makanan"},{"value":"environment","label":"Lingkungan"},{"value":"insect","label":"Serangga"},{"value":"other","label":"Lainnya"}]},
            {"key":"reaksi","label":"Reaksi","type":"text"},
            {"key":"keparahan","label":"Keparahan","type":"select","options":[
                {"value":"ringan","label":"Ringan"},{"value":"sedang","label":"Sedang"},{"value":"berat","label":"Berat"}]},
            {"key":"efek_samping","label":"Efek samping","type":"text"},
            {"key":"tanggal","label":"Tanggal kejadian","type":"date"},
            {"key":"sumber","label":"Sumber","type":"select","options":[
                {"value":"master","label":"Master pasien"},{"value":"catatan","label":"Catatan manual"}]}]},
        {"key":"tanda_vital","label":"Tanda Vital","kind":"vitals","meta":{
            "kriteria":[{"value":"rutin","label":"Rutin"},{"value":"checkup","label":"Checkup"},{"value":"perintah_dpjp","label":"Perintah DPJP"},{"value":"pasca_tindakan","label":"Pasca-tindakan"},{"value":"reassessment_ews","label":"Reassessment-EWS"}],
            "oksigen":[{"value":"udara_ruang","label":"Udara ruang"},{"value":"tambahan","label":"Tambahan"}]}},
        {"key":"antropometri","label":"Antropometri","kind":"antropometri"},
        {"key":"pemeriksaan_fisik","label":"Pemeriksaan Fisik","kind":"physical_exam"},
        {"key":"kesadaran","label":"Kesadaran (GCS)","kind":"consciousness"},
        {"key":"nyeri","label":"Asesmen Nyeri","kind":"repeatable","fields":[
            {"key":"skala","label":"Jenis skala","type":"select","options":[
                {"value":"nrs","label":"NRS (0-10)"},{"value":"fps_r","label":"FPS-R"},{"value":"flacc","label":"FLACC"}]},
            {"key":"skor","label":"Skor","type":"number"},
            {"key":"lokasi","label":"Lokasi","type":"text"},
            {"key":"durasi","label":"Durasi","type":"text"},
            {"key":"frekuensi","label":"Frekuensi","type":"text"},
            {"key":"karakteristik","label":"Karakteristik","type":"text"},
            {"key":"berkurang","label":"Berkurang bila","type":"text"},
            {"key":"penyebab","label":"Penyebab","type":"text"},
            {"key":"lama","label":"Lama nyeri","type":"text"},
            {"key":"poss","label":"POSS","type":"select","options":[
                {"value":"S","label":"S"},{"value":"1","label":"1"},{"value":"2","label":"2"},{"value":"3","label":"3"},{"value":"4","label":"4"}]}]},
        {"key":"penunjang","label":"Catatan & Hasil Penunjang","kind":"textarea"},
        {"key":"narasi_asesmen","label":"Naratif Asesmen","kind":"textarea"},
        {"key":"diagnosa","label":"Diagnosa","kind":"diagnosis","soft_required":true},
        {"key":"tindakan","label":"Tindakan & Prosedur","kind":"repeatable","fields":[
            {"key":"concept_id","label":"Prosedur (ICD-9-CM)","type":"text"},
            {"key":"kode","label":"Kode","type":"text"},
            {"key":"nama","label":"Nama prosedur","type":"text"},
            {"key":"tarif_snapshot","label":"Tarif","type":"number"},
            {"key":"waktu","label":"Waktu","type":"time"},
            {"key":"dokter","label":"Dokter pemeriksa","type":"text"},
            {"key":"cito","label":"CITO","type":"boolean"},
            {"key":"catatan_hasil","label":"Catatan hasil","type":"text"},
            {"key":"lokasi_unit","label":"Lokasi/unit","type":"text"},
            {"key":"petugas_mengorder","label":"Petugas mengorder","type":"text"}]},
        {"key":"keterangan_tindakan","label":"Keterangan tindakan","kind":"textarea"},
        {"key":"instruksi_non_obat","label":"Instruksi Terapi Non-Obat","kind":"textarea"},
        {"key":"rencana_tindak_lanjut","label":"Rencana Tindak Lanjut","kind":"rtl","soft_required":true,"meta":{
            "moda":[{"value":"kontrol_poliklinik","label":"Kontrol poliklinik"},{"value":"panggil_pasien","label":"Panggil pasien"},{"value":"rujuk_fasilitas_lain","label":"Rujuk fasilitas lain"},{"value":"rawat_inap_terencana","label":"Rawat inap terencana"},{"value":"lainnya","label":"Lainnya"}]}},
        {"key":"rekomendasi","label":"Rekomendasi & Jadwal Kontrol","kind":"rekomendasi"}]}'::text)
) AS v(code, sections)
WHERE t.id = f.template_id
  AND t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = v.code;

UPDATE core.template SET version = 3
WHERE kind = 'clinical_form' AND scope = 'platform' AND code = 'soap_dokter';

DO $$ BEGIN
    IF (SELECT t.version FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter') <> 3 THEN
        RAISE EXCEPTION 'rollback soap_dokter v3 gagal';
    END IF;
END $$;

COMMIT;
