-- Kalau ada row core.person.gender IS NULL (mis. Owner yang udah didaftarin
-- pakai fix ini), ALTER ... SET NOT NULL di bawah bakal gagal kena
-- constraint violation sampai row itu dibenerin manual dulu — disengaja,
-- rollback migration ini SEHARUSNYA gak dilakuin tanpa mikir dulu row mana
-- yang bakal kena.
ALTER TABLE core.person ALTER COLUMN gender SET NOT NULL;
