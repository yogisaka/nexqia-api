BEGIN;

UPDATE core.template_clinical_form f
SET sections = v.sections::jsonb
FROM core.template t, (VALUES
    ('soap_dokter', '{"sections":[{"key":"subjective","label":"S — Subjektif","kind":"textarea","soft_required":true},{"key":"objective","label":"O — Objektif (tanda vital)","kind":"vitals"},{"key":"assessment","label":"A — Asesmen (diagnosis)","kind":"diagnosis","soft_required":true},{"key":"plan","label":"P — Rencana","kind":"textarea","soft_required":true}]}'::text)
) AS v(code, sections)
WHERE t.id = f.template_id
  AND t.kind = 'clinical_form' AND t.scope = 'platform' AND t.code = v.code;

UPDATE core.template SET version = 1
WHERE kind = 'clinical_form' AND scope = 'platform' AND code = 'soap_dokter';

DROP TABLE IF EXISTS clinical_form.content_template;

COMMIT;
