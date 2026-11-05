BEGIN;

-- Berkas RM (spec 2026-11-05-berkas-rm-revisi, kutipan spec 2026-11-04 §3):
-- jenis dokumen konteks kunjungan + hasil luar — katalog DB (§7a.2 tingkat d),
-- FE hanya merender.
INSERT INTO core.document_type (code, name, sort_order) VALUES
    ('hasil_lab_luar', 'Hasil Lab Luar', 100),
    ('hasil_radiologi_luar', 'Hasil Radiologi Luar', 110),
    ('surat_sehat', 'Surat Sehat', 120),
    ('surat_keterangan_sakit', 'Surat Keterangan Sakit', 130),
    ('resume_medis_luar', 'Resume Medis Luar', 140)
ON CONFLICT (code) DO NOTHING;

UPDATE core.document_type SET sort_order = 200 WHERE code = 'lainnya';

DO $$ BEGIN
    IF (SELECT count(*) FROM core.document_type
         WHERE code IN ('hasil_lab_luar','hasil_radiologi_luar','surat_sehat',
                        'surat_keterangan_sakit','resume_medis_luar')) <> 5
       OR (SELECT sort_order FROM core.document_type WHERE code = 'lainnya') <> 200 THEN
        RAISE EXCEPTION 'seed document_type berkas rm gagal';
    END IF;
END $$;

COMMIT;
