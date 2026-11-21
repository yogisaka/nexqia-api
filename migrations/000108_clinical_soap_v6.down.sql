BEGIN;

-- Rollback soap_dokter v6 -> v5: hapus field dampak_fungsi & fase dari section nyeri.
-- form_instance yang sudah menyimpan field baru TIDAK dihapus (union; pola 000107:5).

UPDATE core.template_clinical_form f
SET sections = (
    SELECT jsonb_build_object('sections', jsonb_agg(
        CASE WHEN elem->>'key' = 'nyeri' THEN jsonb_set(elem, '{fields}',
            (SELECT jsonb_agg(fld) FROM jsonb_array_elements(elem->'fields') fld
             WHERE fld->>'key' <> 'dampak_fungsi' AND fld->>'key' <> 'fase'))
        ELSE elem END))
    FROM jsonb_array_elements(f.sections->'sections') elem)
FROM core.template t
WHERE t.id = f.template_id
  AND t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter';

UPDATE core.template SET version = 5
WHERE kind = 'clinical_form' AND scope = 'platform' AND code = 'soap_dokter';

COMMIT;
