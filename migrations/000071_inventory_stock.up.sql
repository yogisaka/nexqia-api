-- nexqia-api/migrations/000071_inventory_stock.up.sql
-- Spec 2026-10-07-inventory-17-stock-procurement §3 (#17a).

-- Company-level suppliers (merchant_id NULL) next to merchant-only ones.
ALTER TABLE core.supplier ALTER COLUMN merchant_id DROP NOT NULL;
DROP POLICY supplier_isolation ON core.supplier;
CREATE POLICY supplier_isolation ON core.supplier
    USING (company_id = current_setting('app.current_company_id')::uuid
           AND (merchant_id IS NULL OR merchant_id = current_setting('app.current_merchant_id', true)::uuid));
CREATE UNIQUE INDEX uq_supplier_company_code ON core.supplier (company_id, code) WHERE merchant_id IS NULL AND deleted_at IS NULL;

ALTER TABLE core.location DROP CONSTRAINT location_functions_check;
ALTER TABLE core.location ADD CONSTRAINT location_functions_check
    CHECK (functions <@ ARRAY['practice','nurse_station','counter','ward_room','treatment','support','warehouse','pharmacy']::text[]);

CREATE TABLE inventory.merchant_setting (
    merchant_id                   uuid PRIMARY KEY REFERENCES core.merchant(id),
    company_id                    uuid NOT NULL REFERENCES core.company(id),
    near_expiry_months_1          smallint NOT NULL DEFAULT 3 CHECK (near_expiry_months_1 > 0),
    near_expiry_months_2          smallint NOT NULL DEFAULT 6 CHECK (near_expiry_months_2 >= near_expiry_months_1),
    min_shelf_life_months         smallint NOT NULL DEFAULT 12 CHECK (min_shelf_life_months >= 0),
    min_shelf_life_vaccine_months smallint NOT NULL DEFAULT 6 CHECK (min_shelf_life_vaccine_months >= 0),
    input_vat_creditable          boolean NOT NULL DEFAULT false,
    default_vat_percent           numeric(5,2) NOT NULL DEFAULT 11 CHECK (default_vat_percent >= 0 AND default_vat_percent <= 100),
    stock_locked_until            date,
    updated_at                    timestamptz NOT NULL DEFAULT now(),
    updated_by                    uuid
);
ALTER TABLE inventory.merchant_setting ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_setting_isolation ON inventory.merchant_setting USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_inventory_merchant_setting_tenant_check BEFORE INSERT OR UPDATE ON inventory.merchant_setting FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.lot (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    item_id     uuid NOT NULL REFERENCES inventory.item(id),
    batch_no    text NOT NULL CHECK (btrim(batch_no) <> ''),
    expiry_date date,
    is_recalled boolean NOT NULL DEFAULT false,
    recall_note text,
    created_at  timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_lot_recall CHECK (NOT is_recalled OR btrim(coalesce(recall_note, '')) <> '')
);
CREATE UNIQUE INDEX uq_lot ON inventory.lot (item_id, batch_no, COALESCE(expiry_date, DATE '9999-12-31'));
ALTER TABLE inventory.lot ENABLE ROW LEVEL SECURITY;
CREATE POLICY lot_isolation ON inventory.lot USING (company_id = current_setting('app.current_company_id')::uuid);

CREATE TABLE inventory.stock_balance (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    location_id       uuid NOT NULL REFERENCES core.location(id),
    item_id           uuid NOT NULL REFERENCES inventory.item(id),
    lot_id            uuid NOT NULL REFERENCES inventory.lot(id),
    source            text NOT NULL CHECK (source IN ('purchase','program','grant','consignment')),
    status            text NOT NULL CHECK (status IN ('available','quarantine','damaged')),
    quantity          numeric(14,3) NOT NULL DEFAULT 0 CHECK (quantity >= 0),
    received_first_at timestamptz NOT NULL DEFAULT now(),
    updated_at        timestamptz NOT NULL DEFAULT now(),
    UNIQUE (location_id, lot_id, source, status)
);
CREATE INDEX idx_stock_balance_item ON inventory.stock_balance (merchant_id, item_id) WHERE quantity > 0;
ALTER TABLE inventory.stock_balance ENABLE ROW LEVEL SECURITY;
CREATE POLICY stock_balance_isolation ON inventory.stock_balance USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_stock_balance_tenant_check BEFORE INSERT OR UPDATE ON inventory.stock_balance FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.stock_move (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    move_date       date NOT NULL,
    move_type       text NOT NULL CHECK (move_type IN ('receipt','issue','transfer_out','transfer_in','adjustment','count','status_change','reversal')),
    location_id     uuid NOT NULL REFERENCES core.location(id),
    item_id         uuid NOT NULL REFERENCES inventory.item(id),
    lot_id          uuid NOT NULL REFERENCES inventory.lot(id),
    source          text NOT NULL,
    status          text NOT NULL,
    quantity        numeric(14,3) NOT NULL CHECK (quantity <> 0),
    unit_cost       numeric(19,6) NOT NULL DEFAULT 0 CHECK (unit_cost >= 0),
    ref_type        text,
    ref_id          uuid,
    reason          text,
    override_reason text,
    reversal_of     uuid REFERENCES inventory.stock_move(id),
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid
);
CREATE INDEX idx_stock_move_card ON inventory.stock_move (merchant_id, item_id, location_id, move_date, created_at);
CREATE INDEX idx_stock_move_ref ON inventory.stock_move (ref_type, ref_id);
ALTER TABLE inventory.stock_move ENABLE ROW LEVEL SECURITY;
CREATE POLICY stock_move_isolation ON inventory.stock_move USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_stock_move_tenant_check BEFORE INSERT OR UPDATE ON inventory.stock_move FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON inventory.stock_move FROM app_runtime;

CREATE TABLE inventory.item_cost (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    item_id     uuid NOT NULL REFERENCES inventory.item(id),
    avg_cost    numeric(19,6) NOT NULL DEFAULT 0 CHECK (avg_cost >= 0),
    updated_at  timestamptz NOT NULL DEFAULT now(),
    UNIQUE (merchant_id, item_id)
);
ALTER TABLE inventory.item_cost ENABLE ROW LEVEL SECURITY;
CREATE POLICY item_cost_isolation ON inventory.item_cost USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_item_cost_tenant_check BEFORE INSERT OR UPDATE ON inventory.item_cost FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.goods_receipt (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    supplier_id      uuid REFERENCES core.supplier(id),
    location_id      uuid NOT NULL REFERENCES core.location(id),
    source           text NOT NULL DEFAULT 'purchase' CHECK (source IN ('purchase','program','grant','consignment')),
    invoice_no       text,
    invoice_date     date,
    received_on      date NOT NULL,
    invoice_discount numeric(15,2) NOT NULL DEFAULT 0 CHECK (invoice_discount >= 0),
    freight          numeric(15,2) NOT NULL DEFAULT 0 CHECK (freight >= 0),
    vat_included     boolean NOT NULL DEFAULT true,
    vat_percent      numeric(5,2) NOT NULL DEFAULT 11 CHECK (vat_percent >= 0 AND vat_percent <= 100),
    status           text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','posted','void')),
    posted_at        timestamptz,
    posted_by        uuid,
    void_reason      text,
    note             text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid,
    row_version      integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_goods_receipt_supplier CHECK (source IN ('program','grant') OR supplier_id IS NOT NULL),
    CONSTRAINT chk_goods_receipt_posted CHECK ((status = 'draft') = (posted_at IS NULL)),
    CONSTRAINT chk_goods_receipt_void CHECK ((status = 'void') = (btrim(coalesce(void_reason, '')) <> ''))
);
CREATE TRIGGER trg_goods_receipt_touch BEFORE UPDATE ON inventory.goods_receipt FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.goods_receipt ENABLE ROW LEVEL SECURITY;
CREATE POLICY goods_receipt_isolation ON inventory.goods_receipt USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_goods_receipt_tenant_check BEFORE INSERT OR UPDATE ON inventory.goods_receipt FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.goods_receipt_line (
    id                      uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id              uuid NOT NULL REFERENCES core.company(id),
    merchant_id             uuid NOT NULL REFERENCES core.merchant(id),
    receipt_id              uuid NOT NULL REFERENCES inventory.goods_receipt(id) ON DELETE CASCADE,
    item_id                 uuid NOT NULL REFERENCES inventory.item(id),
    item_unit_id            uuid NOT NULL REFERENCES inventory.item_unit(id),
    pack_qty                numeric(12,3) NOT NULL CHECK (pack_qty > 0),
    pack_content            numeric(12,3) NOT NULL CHECK (pack_content > 0),
    bonus_pack_qty          numeric(12,3) NOT NULL DEFAULT 0 CHECK (bonus_pack_qty >= 0),
    pack_price              numeric(15,2) NOT NULL DEFAULT 0 CHECK (pack_price >= 0),
    line_discount_percent   numeric(5,2) NOT NULL DEFAULT 0 CHECK (line_discount_percent >= 0 AND line_discount_percent <= 100),
    batch_no                text,
    expiry_date             date,
    short_shelf_life_reason text,
    base_qty                numeric(14,3),
    unit_cost               numeric(19,6),
    hna_ppn_per_pack        numeric(15,2),
    lot_id                  uuid REFERENCES inventory.lot(id),
    sort_order              integer NOT NULL DEFAULT 0
);
CREATE INDEX idx_goods_receipt_line_receipt ON inventory.goods_receipt_line (receipt_id);
ALTER TABLE inventory.goods_receipt_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY goods_receipt_line_isolation ON inventory.goods_receipt_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_goods_receipt_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.goods_receipt_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.stock_level (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    location_id uuid NOT NULL REFERENCES core.location(id),
    item_id     uuid NOT NULL REFERENCES inventory.item(id),
    min_qty     numeric(14,3) NOT NULL CHECK (min_qty >= 0),
    max_qty     numeric(14,3) NOT NULL,
    UNIQUE (location_id, item_id),
    CONSTRAINT chk_stock_level_range CHECK (max_qty >= min_qty)
);
ALTER TABLE inventory.stock_level ENABLE ROW LEVEL SECURITY;
CREATE POLICY stock_level_isolation ON inventory.stock_level USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_stock_level_tenant_check BEFORE INSERT OR UPDATE ON inventory.stock_level FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.stock_count (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    location_id uuid NOT NULL REFERENCES core.location(id),
    count_date  date NOT NULL,
    status      text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','posted')),
    note        text,
    posted_at   timestamptz,
    posted_by   uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid
);
CREATE UNIQUE INDEX uq_stock_count_draft ON inventory.stock_count (location_id) WHERE status = 'draft';
ALTER TABLE inventory.stock_count ENABLE ROW LEVEL SECURITY;
CREATE POLICY stock_count_isolation ON inventory.stock_count USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_stock_count_tenant_check BEFORE INSERT OR UPDATE ON inventory.stock_count FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.stock_count_line (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    count_id    uuid NOT NULL REFERENCES inventory.stock_count(id) ON DELETE CASCADE,
    balance_id  uuid NOT NULL REFERENCES inventory.stock_balance(id),
    system_qty  numeric(14,3) NOT NULL,
    counted_qty numeric(14,3) CHECK (counted_qty >= 0),
    reason      text,
    UNIQUE (count_id, balance_id)
);
ALTER TABLE inventory.stock_count_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY stock_count_line_isolation ON inventory.stock_count_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_stock_count_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.stock_count_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000039', 'inventory.stock.receive', 'Terima barang dari faktur supplier', 'inventory'),
    ('00000000-0000-0000-0004-000000000040', 'inventory.stock.manage', 'Mutasi, pemakaian, opname, penyesuaian, dan pengaturan gudang', 'inventory'),
    ('00000000-0000-0000-0004-000000000041', 'inventory.stock.view', 'Lihat saldo stok, kartu stok, dan laporan gudang', 'inventory')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('inventory.stock.receive', 'inventory.stock.manage', 'inventory.stock.view')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('inventory.stock.receive', 'inventory.stock.manage', 'inventory.stock.view')
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
