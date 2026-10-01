-- Company self-registration + Owner bootstrap.
-- See 2026-09-23-saas-registration-owner-bootstrap-design.md

-- §6 poin 13 / Temuan C: session.Issue needs to create a session for a user with
-- NO merchant yet (Owner right after registration). session.Issue/auth.GenerateToken/
-- checkDeviceLimit and the CreateRefreshToken/CountActiveRefreshTokens/
-- ListActiveRefreshTokens queries are already tolerant of pgtype.UUID{Valid:false}
-- (serializes to NULL / a zero-UUID string claim, no crash, no filter requires
-- non-null) — this is the ONLY change needed for that case.
ALTER TABLE core.refresh_token ALTER COLUMN merchant_id DROP NOT NULL;

-- §5/§7: app_user gets its own phone column (NOT core.person.phone, which is
-- shared with patient data — a unique constraint there would break legitimate
-- patient records, e.g. family members sharing one phone number).
ALTER TABLE core.app_user ADD COLUMN phone text;
ALTER TABLE core.app_user ADD CONSTRAINT uq_app_user_email UNIQUE (email);
ALTER TABLE core.app_user ADD CONSTRAINT uq_app_user_phone UNIQUE (phone);

-- §3: company-wide (not merchant-specific) role assignment — lets an Owner with
-- zero merchants still be authorized for company-level actions. Same isolation
-- pattern as core.user_merchant_role (migration 000004).
CREATE TABLE core.user_company_role (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    user_id     uuid NOT NULL REFERENCES core.app_user(id),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    role_id     uuid NOT NULL REFERENCES core.role(id),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    UNIQUE (user_id, company_id, role_id)
);
ALTER TABLE core.user_company_role ENABLE ROW LEVEL SECURITY;
CREATE POLICY user_company_role_isolation ON core.user_company_role
    USING (company_id = current_setting('app.current_company_id')::uuid);

-- §3 poin 4 / Temuan B: user_merchant_role_isolation RLS is scoped to ONE
-- app.current_merchant_id per session (see core.list_user_merchants, migration
-- 000015, for the exact same problem solved the exact same way before). In the
-- companyOnlyAuthed route group (no merchant context ever set), a plain query
-- against user_merchant_role would ERROR, not return empty. SECURITY DEFINER
-- bypasses RLS here, but company_id scoping is explicit in the SQL body itself.
CREATE FUNCTION core.user_has_company_level_permission(p_user_id uuid, p_company_id uuid, p_codes text[])
RETURNS boolean
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
STABLE
AS $$
    SELECT EXISTS (
        SELECT 1
        FROM core.user_company_role ucr
        JOIN core.role_permission rp ON rp.role_id = ucr.role_id
        JOIN core.permission p ON p.id = rp.permission_id
        WHERE ucr.user_id = p_user_id AND ucr.company_id = p_company_id AND p.code = ANY(p_codes)
        UNION ALL
        SELECT 1
        FROM core.user_merchant_role umr
        JOIN core.merchant m ON m.id = umr.merchant_id AND m.company_id = p_company_id AND m.deleted_at IS NULL
        JOIN core.role_permission rp ON rp.role_id = umr.role_id
        JOIN core.permission p ON p.id = rp.permission_id
        WHERE umr.user_id = p_user_id AND p.code = ANY(p_codes)
    );
$$;

GRANT EXECUTE ON FUNCTION core.user_has_company_level_permission(uuid, uuid, text[]) TO app_runtime;

-- §4: split core.company.manage (platform-wide, ListCompanies/CreateCompany only)
-- from a tenant-scoped equivalent for self-company management.
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000024', 'core.company.manage.own', 'Kelola company sendiri (tenant-scoped)', 'core');
