-- nexqia-api/migrations/000072_person_nik_exemption.down.sql
ALTER TABLE core.person
    DROP CONSTRAINT chk_person_nik_or_exemption,
    DROP CONSTRAINT chk_person_nik_exemption_note,
    DROP COLUMN nik_exemption_note,
    DROP COLUMN nik_exemption;
