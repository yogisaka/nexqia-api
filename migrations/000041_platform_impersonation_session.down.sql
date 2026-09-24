-- nexqia-api/migrations/000041_platform_impersonation_session.down.sql
DROP TABLE IF EXISTS platform.impersonation_session;
DROP FUNCTION IF EXISTS platform.get_company(uuid);
DROP FUNCTION IF EXISTS platform.get_app_user_by_username(uuid, text);
