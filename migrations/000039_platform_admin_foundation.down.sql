-- migrations/000039_platform_admin_foundation.down.sql
DROP FUNCTION IF EXISTS platform.get_company_detail(uuid);
DROP FUNCTION IF EXISTS platform.count_companies();
DROP FUNCTION IF EXISTS platform.list_companies(integer, integer);
DROP TABLE IF EXISTS platform.admin_refresh_token;
DROP TABLE IF EXISTS platform.admin_user_role;
DROP TABLE IF EXISTS platform.role_permission;
DROP TABLE IF EXISTS platform.role;
DROP TABLE IF EXISTS platform.permission;
DROP TABLE IF EXISTS platform.admin_user;
DROP SCHEMA IF EXISTS platform;