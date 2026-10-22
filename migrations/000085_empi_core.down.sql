BEGIN;

DELETE FROM core.role_permission WHERE permission_id IN (
    SELECT id FROM core.permission WHERE code IN ('empi.master.view', 'empi.master.manage'));
DELETE FROM core.permission WHERE id IN (
    '00000000-0000-0000-0004-000000000049',
    '00000000-0000-0000-0004-00000000004a');
DROP SCHEMA empi CASCADE;

COMMIT;
