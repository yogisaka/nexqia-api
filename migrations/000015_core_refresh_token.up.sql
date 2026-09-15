-- migrations/000015_core_refresh_token.up.sql
-- Session/refresh-token model, see docs/design/specs/2026-09-15-session-refresh-token-design.md.

ALTER TABLE core.company
    ADD COLUMN max_concurrent_sessions int NOT NULL DEFAULT 1;

CREATE TABLE core.refresh_token (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    user_id       uuid NOT NULL REFERENCES core.app_user(id),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id), -- merchant aktif sesi ini, bukan basis device-limit (lihat spec §3a)
    device_id     text NOT NULL,
    device_label  text NOT NULL,
    token_hash    text NOT NULL,
    issued_at     timestamptz NOT NULL DEFAULT now(),
    last_used_at  timestamptz NOT NULL DEFAULT now(),
    expires_at    timestamptz NOT NULL,
    revoked_at    timestamptz,
    created_ip    inet NOT NULL
);
CREATE UNIQUE INDEX refresh_token_hash_idx ON core.refresh_token (token_hash);
CREATE INDEX refresh_token_user_active_idx ON core.refresh_token (user_id, revoked_at);
ALTER TABLE core.refresh_token ENABLE ROW LEVEL SECURITY;
CREATE POLICY refresh_token_isolation ON core.refresh_token
    USING (company_id = current_setting('app.current_company_id')::uuid);
CREATE TRIGGER trg_refresh_token_tenant_check BEFORE INSERT OR UPDATE ON core.refresh_token
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- Merchant enumeration for login-time merchant-selection (spec §3a). user_merchant_role's
-- RLS is deliberately merchant-scoped (role config isolation across merchants within one
-- company, e.g. post-merger tenants) — a plain query under a single-merchant session
-- would only ever see one merchant. SECURITY DEFINER, owned by the migration/schema owner,
-- bypasses RLS (Postgres table owners are RLS-exempt by default) but returns only
-- id/name — no role or permission data crosses the isolation boundary.
CREATE FUNCTION core.list_user_merchants(p_user_id uuid)
RETURNS TABLE(id uuid, name text)
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    SELECT DISTINCT m.id, m.name
    FROM core.user_merchant_role umr
    JOIN core.merchant m ON m.id = umr.merchant_id AND m.deleted_at IS NULL
    WHERE umr.user_id = p_user_id;
$$ LANGUAGE sql STABLE;
