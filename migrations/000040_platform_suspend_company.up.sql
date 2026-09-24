-- nexqia-api/migrations/000040_platform_suspend_company.up.sql
-- Both functions return boolean (true = a company row was actually found and
-- updated, false = the id didn't match anything) — NOT void. A void-returning
-- UPDATE-with-no-match silently affects 0 rows with no error, which would make
-- the Go handler return 200 OK for a nonexistent company id. See
-- docs/design/specs/2026-09-24-platform-admin-suspend-company-design.md §4.

CREATE FUNCTION platform.suspend_company(p_company_id uuid, p_updated_by uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, pg_temp AS $$
DECLARE
    v_row_count integer;
BEGIN
    UPDATE core.company SET is_active = false, updated_by = p_updated_by
    WHERE id = p_company_id AND deleted_at IS NULL;
    GET DIAGNOSTICS v_row_count = ROW_COUNT;
    IF v_row_count = 0 THEN
        RETURN false;
    END IF;

    UPDATE core.refresh_token SET revoked_at = now()
    WHERE company_id = p_company_id AND revoked_at IS NULL;
    RETURN true;
END;
$$;
GRANT EXECUTE ON FUNCTION platform.suspend_company(uuid, uuid) TO app_runtime;

CREATE FUNCTION platform.activate_company(p_company_id uuid, p_updated_by uuid)
RETURNS boolean
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, pg_temp AS $$
DECLARE
    v_row_count integer;
BEGIN
    UPDATE core.company SET is_active = true, updated_by = p_updated_by
    WHERE id = p_company_id AND deleted_at IS NULL;
    GET DIAGNOSTICS v_row_count = ROW_COUNT;
    RETURN v_row_count > 0;
END;
$$;
GRANT EXECUTE ON FUNCTION platform.activate_company(uuid, uuid) TO app_runtime;
