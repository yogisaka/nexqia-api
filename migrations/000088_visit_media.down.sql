BEGIN;

DROP INDEX IF EXISTS core.uq_person_card_token;
ALTER TABLE core.person
    DROP COLUMN IF EXISTS card_issued_by,
    DROP COLUMN IF EXISTS card_issued_at,
    DROP COLUMN IF EXISTS card_token_hash;
ALTER TABLE operations.admission DROP COLUMN IF EXISTS fall_risk;
DROP TABLE IF EXISTS operations.visit_media_config;
DROP TABLE IF EXISTS operations.visit_token;

COMMIT;
