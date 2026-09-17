-- migrations/000023_core_person_photo_url.down.sql
ALTER TABLE core.person DROP COLUMN photo_url;
