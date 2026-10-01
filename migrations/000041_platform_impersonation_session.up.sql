-- nexqia-api/migrations/000041_platform_impersonation_session.up.sql
-- Audit log of platform-admin impersonation sessions — no RLS (platform.*
-- has none, see migration 000039), and target_user_id/target_company_id are
-- deliberately plain uuid with NO cross-schema FK to core.app_user/core.company
-- (no precedent for cross-schema FK anywhere in this codebase; this row must
-- also survive as a permanent audit record even if the target user/company is
-- later deleted). See 2026-09-24-platform-admin-impersonate-design.md §5.
CREATE TABLE platform.impersonation_session (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    admin_user_id     uuid NOT NULL REFERENCES platform.admin_user(id),
    target_user_id    uuid NOT NULL,
    target_company_id uuid NOT NULL,
    reason            text NOT NULL,
    started_at        timestamptz NOT NULL DEFAULT now(),
    expires_at        timestamptz NOT NULL,
    started_ip        inet NOT NULL
);
CREATE INDEX platform_impersonation_session_admin_idx ON platform.impersonation_session (admin_user_id);
CREATE INDEX platform_impersonation_session_target_idx ON platform.impersonation_session (target_user_id);

-- RLS-bypass reads for ImpersonateHandler (app_runtime is NOBYPASSRLS and
-- PlatformTxMiddleware sets no tenant GUC — direct SELECTs on RLS-protected
-- core.* would return 0 rows in production; test env bypasses RLS as owner
-- which is why tests passed. Same convention as 000039's platform.list_companies.)
CREATE FUNCTION platform.get_company(p_id uuid)
RETURNS TABLE(id uuid, name text, code text, is_active boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, pg_temp AS $$
BEGIN
	RETURN QUERY
		SELECT c.id, c.name, c.code, c.is_active
		FROM core.company c
		WHERE c.id = p_id AND c.deleted_at IS NULL;
END $$;

CREATE FUNCTION platform.get_app_user_by_username(p_company_id uuid, p_username text)
RETURNS TABLE(id uuid, person_id uuid, username text, is_active boolean)
LANGUAGE plpgsql SECURITY DEFINER SET search_path = core, pg_temp AS $$
BEGIN
	RETURN QUERY
		SELECT u.id, u.person_id, u.username, u.is_active
		FROM core.app_user u
		WHERE u.company_id = p_company_id AND u.username = p_username AND u.deleted_at IS NULL;
END $$;

GRANT EXECUTE ON FUNCTION platform.get_company(uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION platform.get_app_user_by_username(uuid, text) TO app_runtime;
