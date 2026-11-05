BEGIN;

DELETE FROM core.document_type
WHERE code IN ('hasil_lab_luar','hasil_radiologi_luar','surat_sehat',
               'surat_keterangan_sakit','resume_medis_luar');
UPDATE core.document_type SET sort_order = 90 WHERE code = 'lainnya';

COMMIT;
