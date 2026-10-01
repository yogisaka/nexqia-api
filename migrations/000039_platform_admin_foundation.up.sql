-- migrations/000039_platform_admin_foundation.up.sql
CREATE SCHEMA IF NOT EXISTS platform;

-- No RLS on any table below — this schema has exactly one population (NEXQIA's
-- own operator team), not per-tenant data. Isolation is enforced in Go
-- (RequirePlatformPermission), not Postgres session GUCs. See
-- 2026-09-24-platform-admin-foundation-design.md §3.

CREATE TABLE platform.admin_user (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    username       text NOT NULL UNIQUE,
    email          text NOT NULL UNIQUE,
    full_name      text NOT NULL,
    password_hash  text NOT NULL,
    is_active      boolean NOT NULL DEFAULT true,
    last_login_at  timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid REFERENCES platform.admin_user(id),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     uuid REFERENCES platform.admin_user(id),
    deleted_at     timestamptz,
    deleted_by     uuid REFERENCES platform.admin_user(id),
    row_version    integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_platform_admin_user_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE TRIGGER trg_platform_admin_user_touch BEFORE UPDATE ON platform.admin_user
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

CREATE TABLE platform.permission (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    code         text NOT NULL UNIQUE,
    description  text NOT NULL,
    module       text NOT NULL
);

CREATE TABLE platform.role (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    name         text NOT NULL UNIQUE,
    description  text,
    is_system    boolean NOT NULL DEFAULT false,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid REFERENCES platform.admin_user(id),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid REFERENCES platform.admin_user(id),
    deleted_at   timestamptz,
    deleted_by   uuid REFERENCES platform.admin_user(id),
    row_version  integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_platform_role_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE TRIGGER trg_platform_role_touch BEFORE UPDATE ON platform.role
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

CREATE TABLE platform.role_permission (
    role_id       uuid NOT NULL REFERENCES platform.role(id),
    permission_id uuid NOT NULL REFERENCES platform.permission(id),
    PRIMARY KEY (role_id, permission_id)
);

CREATE TABLE platform.admin_user_role (
    admin_user_id uuid NOT NULL REFERENCES platform.admin_user(id),
    role_id       uuid NOT NULL REFERENCES platform.role(id),
    PRIMARY KEY (admin_user_id, role_id)
);

CREATE TABLE platform.admin_refresh_token (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    admin_user_id uuid NOT NULL REFERENCES platform.admin_user(id),
    device_id     text NOT NULL,
    device_label  text NOT NULL,
    token_hash    text NOT NULL,
    issued_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    created_ip    inet NOT NULL
);
CREATE UNIQUE INDEX platform_admin_refresh_token_hash_idx ON platform.admin_refresh_token (token_hash);
CREATE INDEX platform_admin_refresh_token_active_idx ON platform.admin_refresh_token (admin_user_id, revoked_at);

-- app_runtime connects directly for platform-admin CRUD (login by username, refresh-token
-- rotation, RBAC permission checks via role_permission/admin_user_role) — the SECURITY
-- DEFINER functions below are ONLY for cross-tenant core.company reads, they do NOT cover
-- direct access to platform.* tables themselves. Same grant shape as
-- migrations/000026_app_runtime_operations_grants.up.sql.
GRANT USAGE ON SCHEMA platform TO app_runtime;
GRANT SELECT, INSERT, UPDATE, DELETE ON ALL TABLES IN SCHEMA platform TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA platform GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA platform GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;
GRANT USAGE, SELECT ON ALL SEQUENCES IN SCHEMA platform TO app_runtime;

-- RLS-bypass reads for the platform-admin dashboard (spec §5) — SECURITY DEFINER,
-- explicit search_path (defense against search-path hijacking), fully qualified
-- table names throughout, granted only to app_runtime (same connection every
-- other handler uses — no separate DB role/connection pool needed).

CREATE FUNCTION platform.list_companies(p_limit integer, p_offset integer)
RETURNS SETOF core.company
LANGUAGE sql SECURITY DEFINER SET search_path = core, pg_temp AS $$
    SELECT * FROM core.company
    WHERE deleted_at IS NULL
    ORDER BY created_at DESC
    LIMIT p_limit OFFSET p_offset;
$$;
GRANT EXECUTE ON FUNCTION platform.list_companies(integer, integer) TO app_runtime;

CREATE FUNCTION platform.count_companies()
RETURNS bigint
LANGUAGE sql SECURITY DEFINER SET search_path = core, pg_temp AS $$
    SELECT count(*) FROM core.company WHERE deleted_at IS NULL;
$$;
GRANT EXECUTE ON FUNCTION platform.count_companies() TO app_runtime;

CREATE FUNCTION platform.get_company_detail(p_company_id uuid)
RETURNS TABLE (
    id uuid, code text, name text, is_active boolean,
    created_at timestamptz, updated_at timestamptz,
    merchant_count bigint, user_count bigint
)
LANGUAGE sql SECURITY DEFINER SET search_path = core, pg_temp AS $$
    SELECT c.id, c.code, c.name, c.is_active, c.created_at, c.updated_at,
           COALESCE(m.cnt, 0), COALESCE(u.cnt, 0)
    FROM core.company c
    LEFT JOIN (SELECT company_id, count(*) cnt FROM core.merchant WHERE deleted_at IS NULL GROUP BY company_id) m ON m.company_id = c.id
    LEFT JOIN (SELECT company_id, count(*) cnt FROM core.app_user WHERE deleted_at IS NULL GROUP BY company_id) u ON u.company_id = c.id
    WHERE c.id = p_company_id AND c.deleted_at IS NULL;
$$;
GRANT EXECUTE ON FUNCTION platform.get_company_detail(uuid) TO app_runtime;