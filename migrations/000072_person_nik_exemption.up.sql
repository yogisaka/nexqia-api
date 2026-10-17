-- nexqia-api/migrations/000072_person_nik_exemption.up.sql
-- Spec 2026-10-07-registration-validation §3.
ALTER TABLE core.person
    ADD COLUMN nik_exemption text CHECK (nik_exemption IN ('newborn','foreign_national','unidentified','other')),
    ADD COLUMN nik_exemption_note text,
    ADD CONSTRAINT chk_person_nik_exemption_note CHECK (nik_exemption IS DISTINCT FROM 'other' OR btrim(coalesce(nik_exemption_note, '')) <> ''),
    ADD CONSTRAINT chk_person_nik_or_exemption CHECK (nik IS NULL OR nik_exemption IS NULL);
