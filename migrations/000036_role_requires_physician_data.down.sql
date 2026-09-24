DELETE FROM core.permission WHERE code IN ('core.user.manage', 'core.role.manage');
ALTER TABLE core.role DROP COLUMN requires_physician_data;
