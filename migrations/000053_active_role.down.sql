DROP FUNCTION IF EXISTS core.user_role_assigned(uuid, uuid, uuid);
DROP FUNCTION IF EXISTS core.default_user_role(uuid, uuid);
ALTER TABLE core.refresh_token DROP COLUMN IF EXISTS active_role_id;
