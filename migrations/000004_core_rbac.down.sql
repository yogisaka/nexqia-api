-- migrations/000004_core_rbac.down.sql
DROP TABLE IF EXISTS core.user_merchant_role;
DROP TABLE IF EXISTS core.role_permission;
DROP TABLE IF EXISTS core.role;
DROP TABLE IF EXISTS core.permission;
DROP TABLE IF EXISTS core.app_user;
