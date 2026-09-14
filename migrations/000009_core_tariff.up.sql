-- migrations/000009_core_tariff.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §7 "Tarif engine".

CREATE TABLE core.service_item (    -- katalog layanan/barang yang bisa ditagih
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    code         text NOT NULL,
    name         text NOT NULL,
    item_type    text NOT NULL CHECK (item_type IN ('procedure','drug','material','room','package')),
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_service_item_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_service_item_merchant ON core.service_item (merchant_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_service_item_touch BEFORE UPDATE ON core.service_item
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.service_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY service_item_isolation ON core.service_item
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_service_item_tenant_check BEFORE INSERT OR UPDATE ON core.service_item
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.rate_component (  -- komponen tarif: jasa RS, jasa dokter, BHP, dst — data, bukan kolom statis
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    code         text NOT NULL,
    name         text NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_rate_component_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE TRIGGER trg_rate_component_touch BEFORE UPDATE ON core.rate_component
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.rate_component ENABLE ROW LEVEL SECURITY;
CREATE POLICY rate_component_isolation ON core.rate_component
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_rate_component_tenant_check BEFORE INSERT OR UPDATE ON core.rate_component
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.service_rate (    -- harga per service_item per payer_class per periode
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    merchant_id        uuid NOT NULL REFERENCES core.merchant(id),
    service_item_id    uuid NOT NULL REFERENCES core.service_item(id),
    rate_component_id  uuid NOT NULL REFERENCES core.rate_component(id),
    payer_class        text NOT NULL DEFAULT 'general',  -- 'general','bpjs_class1', dst
    amount             numeric(15,2) NOT NULL,
    effective_from     date NOT NULL,
    effective_to       date,
    created_at         timestamptz NOT NULL DEFAULT now(),
    created_by         uuid,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    updated_by         uuid,
    deleted_at         timestamptz,
    deleted_by         uuid,
    row_version        integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_service_rate_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_service_rate_lookup ON core.service_rate
    (merchant_id, service_item_id, payer_class, effective_from) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_service_rate_touch BEFORE UPDATE ON core.service_rate
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.service_rate ENABLE ROW LEVEL SECURITY;
CREATE POLICY service_rate_isolation ON core.service_rate
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_service_rate_tenant_check BEFORE INSERT OR UPDATE ON core.service_rate
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
