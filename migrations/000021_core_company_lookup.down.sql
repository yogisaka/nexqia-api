-- migrations/000021_core_company_lookup.down.sql
DROP FUNCTION IF EXISTS core.lookup_company_by_code(text);
