-- nexqia-api/migrations/000067_inventory_items.down.sql
UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
DELETE FROM core.template_role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000035';
DELETE FROM core.role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000035';
DELETE FROM core.permission WHERE id = '00000000-0000-0000-0004-000000000035';
DROP SCHEMA inventory CASCADE;
