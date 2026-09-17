-- migrations/000023_core_person_photo_url.up.sql
-- Additive column beyond docs/07-core-ddl.md's original transcript, same
-- pattern as 000017_core_app_user_pin (pin_hash) — patient/physician/current
-- app_user photos all resolve through core.person, so one column covers all
-- three UI avatar needs. Nullable: no upload flow exists yet.
ALTER TABLE core.person ADD COLUMN photo_url text;
