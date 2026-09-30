-- nexqia-api/migrations/000051_role_templates.down.sql
-- Roles already copied into companies stay (copies, not links).
DROP TABLE IF EXISTS core.template_application;
DROP TABLE IF EXISTS core.template_role_permission;
DROP TABLE IF EXISTS core.template_role;
DROP TABLE IF EXISTS core.template;
