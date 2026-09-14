-- migrations/000010_core_payer.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §8 "Payer / Penjamin".

-- B2B: mitra pengadaan (jual barang/jasa KE rumah sakit) — beda arah dari core.payer (penjamin BAYAR ke rumah sakit)
-- dulu 1 tabel m_company generik (jns_company discriminator: supplier/asuransi/dll) — sisi asuransi udah pecah ke core.payer,
-- ini sisa sisi supplier: dipakai procurement (rebuild/09-connect-ddl belum, cek operations.purchase_order) SEKALIGUS invoicing/AP nanti
CREATE TABLE core.supplier (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    merchant_id        uuid NOT NULL REFERENCES core.merchant(id),
    code               text NOT NULL,
    name               text NOT NULL,
    pic_name           text,
    pic_phone          text,
    address            text,
    npwp               text,                   -- wajib buat invoicing/faktur pajak
    bank_account       jsonb,                   -- [{bank, account_no, account_name}], bisa >1 rekening
    credit_limit       numeric(15,2),
    lead_time_days     integer,                 -- estimasi waktu kirim normal
    lead_time_cito_days integer,                -- estimasi waktu kirim cito/urgent
    payment_term_days  integer,                 -- termin tempo pembayaran (PO tempo)
    contract_start_date date,                   -- MOU/kontrak kerja sama
    contract_end_date  date,
    is_active          boolean NOT NULL DEFAULT true,
    created_at         timestamptz NOT NULL DEFAULT now(),
    created_by         uuid,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    updated_by         uuid,
    deleted_at         timestamptz,
    deleted_by         uuid,
    row_version        integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_supplier_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_supplier_merchant ON core.supplier (merchant_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_supplier_touch BEFORE UPDATE ON core.supplier
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.supplier ENABLE ROW LEVEL SECURITY;
CREATE POLICY supplier_isolation ON core.supplier
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_supplier_tenant_check BEFORE INSERT OR UPDATE ON core.supplier
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- pabrikan/produsen barang (beda dari supplier — supplier yang jual, manufacturer yang bikin, bisa many-to-many via distributor)
-- dipakai buat traceability produk (terutama obat, terkait BPOM) — referensi ringan, bukan mitra transaksi
CREATE TABLE core.manufacturer (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    code         text NOT NULL,
    name         text NOT NULL,
    abbreviation text,
    address      text,
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code)
);
CREATE TRIGGER trg_manufacturer_touch BEFORE UPDATE ON core.manufacturer
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.manufacturer ENABLE ROW LEVEL SECURITY;
CREATE POLICY manufacturer_isolation ON core.manufacturer
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_manufacturer_tenant_check BEFORE INSERT OR UPDATE ON core.manufacturer
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- katalog diet rumah sakit (mis. "Diet DM 1700 kkal", "Rendah Garam", "TKTP") — per merchant, beda RS beda daftar
-- dipakai clinical.nutrition_order; lookup terstruktur biar bisa dianalisis tren (bukan free text)
CREATE TABLE core.diet_type (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    code            text NOT NULL,
    name            text NOT NULL,
    default_calorie numeric(6,2),
    is_active       boolean NOT NULL DEFAULT true,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      uuid,
    row_version     integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code)
);
CREATE TRIGGER trg_diet_type_touch BEFORE UPDATE ON core.diet_type
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.diet_type ENABLE ROW LEVEL SECURITY;
CREATE POLICY diet_type_isolation ON core.diet_type
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_diet_type_tenant_check BEFORE INSERT OR UPDATE ON core.diet_type
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.payer (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    code          text NOT NULL,
    name          text NOT NULL,
    payer_type    text NOT NULL CHECK (payer_type IN ('bpjs','private_insurance','company_guarantor','self_pay')),
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    deleted_at    timestamptz,
    deleted_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_payer_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_payer_merchant ON core.payer (merchant_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_payer_touch BEFORE UPDATE ON core.payer
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.payer ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_isolation ON core.payer
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_tenant_check BEFORE INSERT OR UPDATE ON core.payer
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- pengganti m_company_collateral_blacklist_procedure + m_company_collateral_discount (2 tabel lama -> 1, rule_type discriminator)
CREATE TABLE core.payer_service_rule (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    payer_id         uuid NOT NULL REFERENCES core.payer(id),
    service_item_id  uuid NOT NULL REFERENCES core.service_item(id),
    rule_type        text NOT NULL CHECK (rule_type IN ('blacklist','discount')),
    discount_percent numeric(5,2),    -- diisi kalau rule_type='discount', persen
    discount_amount  numeric(15,2),   -- diisi kalau rule_type='discount', nominal flat (alternatif ke persen)
    is_active        boolean NOT NULL DEFAULT true,
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid,
    row_version      integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, payer_id, service_item_id, rule_type)
);
CREATE INDEX idx_payer_service_rule_lookup ON core.payer_service_rule (merchant_id, payer_id, rule_type) WHERE is_active = true;
CREATE TRIGGER trg_payer_service_rule_touch BEFORE UPDATE ON core.payer_service_rule
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.payer_service_rule ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_service_rule_isolation ON core.payer_service_rule
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_service_rule_tenant_check BEFORE INSERT OR UPDATE ON core.payer_service_rule
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
