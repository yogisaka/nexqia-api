-- Down migration 000098_gizi_template (plan 2026-11-13-gizi-nutrition-care-chunk-2-adime-core Task 1).

DELETE FROM core.template_clinical_form
WHERE form_type IN ('asuhan_gizi','ases_gizi_mnasf');

DELETE FROM core.template
WHERE code IN ('asuhan_gizi','ases_gizi_mnasf')
  AND scope = 'platform' AND kind = 'clinical_form';

DO $$
DECLARE n integer;
BEGIN
    SELECT count(*) INTO n
    FROM core.template t
    JOIN core.template_clinical_form f ON f.template_id = t.id
    WHERE t.scope = 'platform' AND t.kind = 'clinical_form' AND t.is_active;
    IF n <> 7 THEN
        RAISE EXCEPTION 'seed template clinical_form tidak lengkap: % dari 7', n;
    END IF;
END $$;
