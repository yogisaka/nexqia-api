BEGIN;

DROP TRIGGER trg_anthropometry_snapshot ON clinical.anthropometry;
DROP FUNCTION IF EXISTS clinical.fn_anthropometry_snapshot();
DROP TABLE IF EXISTS clinical.nutrition_screening_result, clinical.nutrition_profile_change, clinical.nutrition_episode, clinical.anthropometry CASCADE;
DROP TABLE IF EXISTS ref.growth_status_band, ref.growth_curve;
DROP SCHEMA IF EXISTS ref;

COMMIT;
