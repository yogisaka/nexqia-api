-- migrations/000021_core_company_lookup.up.sql
-- Public, pre-tenant-context lookup used by the login screen to resolve a
-- human-typeable 6-digit company code into X-Company-ID. SECURITY DEFINER
-- bypasses core.company's RLS (company_isolation policy) the same way
-- core.list_user_merchants bypasses core.user_merchant_role's — see
-- 2026-09-16-his-auth-wiring-design.md §3.
CREATE FUNCTION core.lookup_company_by_code(p_code text)
RETURNS SETOF core.company
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    SELECT c.*
    FROM core.company c
    WHERE c.code = p_code AND c.is_active AND c.deleted_at IS NULL;
$$;

GRANT EXECUTE ON FUNCTION core.lookup_company_by_code(text) TO app_runtime;
