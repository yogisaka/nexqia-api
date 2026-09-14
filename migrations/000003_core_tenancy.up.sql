-- migrations/000003_core_tenancy.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §1 "Tenancy root".

CREATE TABLE core.company (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    code         text NOT NULL UNIQUE,       -- short code, dipakai subdomain/tenant routing
    name         text NOT NULL,
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_company_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE TRIGGER trg_company_touch BEFORE UPDATE ON core.company
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.company ENABLE ROW LEVEL SECURITY;
CREATE POLICY company_isolation ON core.company
    USING (id = current_setting('app.current_company_id')::uuid);

CREATE TABLE core.merchant (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    code         text NOT NULL,              -- unik per company
    name         text NOT NULL,              -- nama RS/klinik
    kemkes_facility_code text,               -- kode fasyankes Kemenkes, wajib SATUSEHAT
    bpjs_ppk_code text,                       -- kode PPK BPJS
    timezone     text NOT NULL DEFAULT 'Asia/Jakarta',
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (company_id, code),
    CONSTRAINT chk_merchant_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_merchant_company ON core.merchant (company_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_merchant_touch BEFORE UPDATE ON core.merchant
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.merchant ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_isolation ON core.merchant
    USING (company_id = current_setting('app.current_company_id')::uuid);
