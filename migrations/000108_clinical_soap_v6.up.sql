BEGIN;

-- CPPT: template soap_dokter v5 -> v6. Section nyeri: field dampak_fungsi + fase (select).
-- TIDAK menghapus form_instance (union validator menjaga instance lama valid; spec §3.2).

-- 1. Guard: template platform soap_dokter tepat satu & versi 5.
DO $$ BEGIN
    IF (SELECT count(*) FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter'
          AND t.version = 5) <> 1 THEN
        RAISE EXCEPTION 'seed soap_dokter v6 gagal';
    END IF;
END $$;

-- 2. Tambah field dampak_fungsi + fase pada nyeri hanya bila belum ada.
UPDATE core.template_clinical_form f
SET sections = (
    SELECT jsonb_build_object('sections', jsonb_agg(
        CASE WHEN elem->>'key' = 'nyeri'
              AND NOT (elem->'fields') @> '[{"key":"dampak_fungsi"}]'::jsonb
        THEN jsonb_set(elem, '{fields}',
            (elem->'fields') || '[{"key":"dampak_fungsi","label":"Dampak fungsi","type":"select","options":[{"value":"tidak_mengganggu","label":"Tidak mengganggu aktivitas"},{"value":"ganggu_tidur","label":"Mengganggu tidur"},{"value":"ganggu_aktivitas","label":"Mengganggu aktivitas sehari-hari"},{"value":"ganggu_kerja","label":"Mengganggu kerja/sekolah"}]},{"key":"fase","label":"Fase asesmen","type":"select","options":[{"value":"awal","label":"Asesmen awal"},{"value":"reasesmen","label":"Re-assessment"}]}]'::jsonb)
        ELSE elem END))
    FROM jsonb_array_elements(f.sections->'sections') elem)
FROM core.template t
WHERE t.id = f.template_id
  AND t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter';

-- 3. Bump versi template.
UPDATE core.template SET version = 6
WHERE kind = 'clinical_form' AND scope = 'platform' AND code = 'soap_dokter';

-- 4. Verifikasi pasca-apply.
DO $$ BEGIN
    IF (SELECT t.version FROM core.template t
        WHERE t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = 'soap_dokter') <> 6 THEN
        RAISE EXCEPTION 'seed soap_dokter v6 gagal';
    END IF;
    IF NOT EXISTS (
        SELECT 1 FROM core.template_clinical_form f
        JOIN core.template t ON t.id = f.template_id
        WHERE t.code = 'soap_dokter'
          AND f.sections->'sections' @> '[{"key":"nyeri","fields":[{"key":"dampak_fungsi","type":"select"},{"key":"fase","type":"select"}]}]'::jsonb)
    THEN RAISE EXCEPTION 'seed soap_dokter v6 gagal: patch nyeri tidak efektif';
    END IF;
END $$;

COMMIT;
