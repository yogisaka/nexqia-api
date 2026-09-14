-- migrations/000004_core_rbac.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §2 "RBAC".

CREATE TABLE core.app_user (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id     uuid NOT NULL REFERENCES core.company(id),
    person_id      uuid,                      -- FK forward-ref, core.person belum ada di sini — FK ditambah via ALTER di #4
    username       text NOT NULL,
    email          text,
    password_hash  text NOT NULL,
    mfa_secret     text,                      -- nullable, MFA opsional/wajib via feature_flag
    is_active      boolean NOT NULL DEFAULT true,
    last_login_at  timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     uuid,
    deleted_at     timestamptz,
    deleted_by     uuid,
    row_version    integer NOT NULL DEFAULT 1,
    UNIQUE (company_id, username),
    CONSTRAINT chk_user_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_user_company ON core.app_user (company_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_user_touch BEFORE UPDATE ON core.app_user
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.app_user ENABLE ROW LEVEL SECURITY;
CREATE POLICY user_isolation ON core.app_user
    USING (company_id = current_setting('app.current_company_id')::uuid);

-- katalog permission GLOBAL (fixed, dideploy via migration bukan per-tenant)
CREATE TABLE core.permission (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    code         text NOT NULL UNIQUE,        -- "billing.invoice.create", "clinical.order.write"
    description  text NOT NULL,
    module       text NOT NULL                -- grouping tampilan UI: "billing","clinical", dst
);

CREATE TABLE core.role (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    name         text NOT NULL,
    description  text,
    is_system    boolean NOT NULL DEFAULT false,  -- role bawaan (Admin, Dokter, Kasir) tidak bisa dihapus
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (company_id, name),
    CONSTRAINT chk_role_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE TRIGGER trg_role_touch BEFORE UPDATE ON core.role
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.role ENABLE ROW LEVEL SECURITY;
CREATE POLICY role_isolation ON core.role
    USING (company_id = current_setting('app.current_company_id')::uuid);

CREATE TABLE core.role_permission (
    role_id       uuid NOT NULL REFERENCES core.role(id),
    permission_id uuid NOT NULL REFERENCES core.permission(id),
    PRIMARY KEY (role_id, permission_id)
);

-- assignment role KE user, di-scope per merchant (1 user bisa beda role di beda RS dlm 1 company)
CREATE TABLE core.user_merchant_role (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    user_id      uuid NOT NULL REFERENCES core.app_user(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    role_id      uuid NOT NULL REFERENCES core.role(id),
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    UNIQUE (user_id, merchant_id, role_id)
);
CREATE INDEX idx_umr_user ON core.user_merchant_role (user_id);
CREATE INDEX idx_umr_merchant ON core.user_merchant_role (merchant_id);
ALTER TABLE core.user_merchant_role ENABLE ROW LEVEL SECURITY;
CREATE POLICY user_merchant_role_isolation ON core.user_merchant_role
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
