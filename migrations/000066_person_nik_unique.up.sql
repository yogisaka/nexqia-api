-- nexqia-api/migrations/000066_person_nik_unique.up.sql
-- Spec 2026-10-07-merge-medical-records §4.7: uq_person_nik sits on random
-- pgp_sym_encrypt ciphertext and never matched; uniqueness moves to the
-- deterministic HMAC hash.
DO $$
DECLARE
    v_groups int;
BEGIN
    SELECT count(*) INTO v_groups FROM (
        SELECT company_id, nik_search_hash FROM core.person
        WHERE deleted_at IS NULL AND nik_search_hash IS NOT NULL
        GROUP BY company_id, nik_search_hash HAVING count(*) > 1) d;
    IF v_groups > 0 THEN
        RAISE EXCEPTION '% group(s) of live patients share a NIK. Merge them in /patients/duplicates (pairs scored 100, "NIK sama"), then run migrate-up again.', v_groups;
    END IF;
END $$;
DROP INDEX IF EXISTS core.uq_person_nik;
CREATE UNIQUE INDEX uq_person_nik_hash ON core.person (company_id, nik_search_hash)
    WHERE deleted_at IS NULL AND nik_search_hash IS NOT NULL;
