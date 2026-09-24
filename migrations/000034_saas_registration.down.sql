DELETE FROM core.permission WHERE id = '00000000-0000-0000-0004-000000000024';
DROP FUNCTION IF EXISTS core.user_has_company_level_permission(uuid, uuid, text[]);
DROP TABLE IF EXISTS core.user_company_role;
ALTER TABLE core.app_user DROP CONSTRAINT IF EXISTS uq_app_user_phone;
ALTER TABLE core.app_user DROP CONSTRAINT IF EXISTS uq_app_user_email;
ALTER TABLE core.app_user DROP COLUMN IF EXISTS phone;
ALTER TABLE core.refresh_token ALTER COLUMN merchant_id SET NOT NULL;
