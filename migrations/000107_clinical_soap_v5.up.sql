BEGIN;

-- CPPT: template soap_dokter v4 -> v5. Section nyeri: field lokasi text -> text_multi
-- (union string|string[], K14) + field marks (canvas_marks, figures a1|a2b, K16).
-- TIDAK menghapus form_instance (union validator menjaga instance lama valid; spec §3.2).

-- 1. Guard: template platform soap_dokter tepat satu & versi 4.
DO $$ BEGIN
    IF (SELECT count(*) FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter'
          AND t.version = 4) <> 1 THEN
        RAISE EXCEPTION 'seed soap_dokter v5 gagal';
    END IF;
END $$;

-- 2. Patch section nyeri (by key, bukan index): lokasi.type = text_multi.
UPDATE core.template_clinical_form f
SET sections = (
    SELECT jsonb_build_object('sections', jsonb_agg(
        CASE WHEN elem->>'key' = 'nyeri' THEN jsonb_set(elem, '{fields}',
            (SELECT jsonb_agg(
                CASE WHEN fld->>'key' = 'lokasi' THEN fld || '{"type":"text_multi"}'::jsonb
                     ELSE fld END)
             FROM jsonb_array_elements(elem->'fields') fld))
        ELSE elem END))
    FROM jsonb_array_elements(f.sections->'sections') elem)
FROM core.template t
WHERE t.id = f.template_id
  AND t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter';

-- 3. Tambah field marks pada nyeri hanya bila belum ada.
UPDATE core.template_clinical_form f
SET sections = (
    SELECT jsonb_build_object('sections', jsonb_agg(
        CASE WHEN elem->>'key' = 'nyeri'
              AND NOT (elem->'fields') @> '[{"key":"marks"}]'::jsonb
        THEN jsonb_set(elem, '{fields}',
            (elem->'fields') || '{"key":"marks","label":"Gambar lokasi","type":"canvas_marks","figures":["a1","a2b"]}'::jsonb)
        ELSE elem END))
    FROM jsonb_array_elements(f.sections->'sections') elem)
FROM core.template t
WHERE t.id = f.template_id
  AND t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter';

-- 4. Bump versi template.
UPDATE core.template SET version = 5
WHERE kind = 'clinical_form' AND scope = 'platform' AND code = 'soap_dokter';

-- 5. Verifikasi pasca-apply.
DO $$ BEGIN
    IF (SELECT t.version FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter') <> 5 THEN
        RAISE EXCEPTION 'seed soap_dokter v5 gagal';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM core.template_clinical_form f
        JOIN core.template t ON t.id = f.template_id
        WHERE t.code = 'soap_dokter'
          AND f.sections->'sections' @> '[{"key":"nyeri","fields":[{"key":"lokasi","type":"text_multi"},{"key":"marks","type":"canvas_marks"}]}]'::jsonb)
    THEN RAISE EXCEPTION 'seed soap_dokter v5 gagal: patch nyeri tidak efektif';
    END IF;
END $$;

COMMIT;
