-- nexqia-api/migrations/000040_platform_suspend_company.down.sql
DROP FUNCTION IF EXISTS platform.activate_company(uuid, uuid);
DROP FUNCTION IF EXISTS platform.suspend_company(uuid, uuid);
