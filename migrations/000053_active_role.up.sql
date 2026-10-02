-- Active role per session (spec 2026-10-01-active-role-scope-design §3.1).
ALTER TABLE core.refresh_token
    ADD COLUMN active_role_id uuid REFERENCES core.role(id);

-- Default role = first assigned, non-deleted role at the merchant by name.
-- SECURITY DEFINER: login/refresh run before app.current_merchant_id is set,
-- and core.user_merchant_role RLS depends on it (same pattern as list_user_merchants).
CREATE FUNCTION core.default_user_role(p_user_id uuid, p_merchant_id uuid)
RETURNS uuid
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
STABLE
AS $$
    SELECT r.id
    FROM core.user_merchant_role umr
    JOIN core.role r ON r.id = umr.role_id AND r.deleted_at IS NULL
    WHERE umr.user_id = p_user_id AND umr.merchant_id = p_merchant_id
    ORDER BY lower(r.name), r.id
    LIMIT 1;
$$;

CREATE FUNCTION core.user_role_assigned(p_user_id uuid, p_merchant_id uuid, p_role_id uuid)
RETURNS boolean
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
STABLE
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM core.user_merchant_role umr
        JOIN core.role r ON r.id = umr.role_id AND r.deleted_at IS NULL
        WHERE umr.user_id = p_user_id AND umr.merchant_id = p_merchant_id AND umr.role_id = p_role_id
    );
$$;

GRANT EXECUTE ON FUNCTION core.default_user_role(uuid, uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.user_role_assigned(uuid, uuid, uuid) TO app_runtime;
