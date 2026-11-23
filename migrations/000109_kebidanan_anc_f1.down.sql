BEGIN;
DROP TABLE IF EXISTS clinical.person_riwayat_vaksin, ref.immunization_schedule, clinical.midwifery_usg, clinical.midwifery_pe_screen, clinical.midwifery_anc_visit, clinical.midwifery_pregnancy, operations.care_episode CASCADE;
COMMIT;
