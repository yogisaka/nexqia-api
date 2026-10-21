-- nexqia-api/migrations/000080_procurement_core.up.sql
-- Spec 2026-10-20-inventory-17b-procurement-special-cases §4.1 (P1).

CREATE TABLE inventory.procurement_setting (
    company_id               uuid PRIMARY KEY REFERENCES core.company(id),
    mode                     text NOT NULL DEFAULT 'desentral' CHECK (mode IN ('desentral','sentral','hibrid')),
    headquarters_merchant_id uuid REFERENCES core.merchant(id),
    updated_at               timestamptz NOT NULL DEFAULT now(),
    updated_by               uuid,
    CONSTRAINT chk_procurement_setting_mode CHECK (mode = 'desentral' OR headquarters_merchant_id IS NOT NULL)
);
ALTER TABLE inventory.procurement_setting ENABLE ROW LEVEL SECURITY;
CREATE POLICY procurement_setting_isolation ON inventory.procurement_setting
    USING (company_id = current_setting('app.current_company_id')::uuid);

CREATE TABLE inventory.procurement_central_item (
    id         uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id uuid NOT NULL REFERENCES core.company(id),
    item_id    uuid NOT NULL REFERENCES inventory.item(id),
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid,
    CONSTRAINT uq_procurement_central_item UNIQUE (company_id, item_id)
);
ALTER TABLE inventory.procurement_central_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY procurement_central_item_isolation ON inventory.procurement_central_item
    USING (company_id = current_setting('app.current_company_id')::uuid);

CREATE TABLE inventory.contract_price (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    supplier_id  uuid NOT NULL REFERENCES core.supplier(id),
    item_id      uuid NOT NULL REFERENCES inventory.item(id),
    item_unit_id uuid NOT NULL REFERENCES inventory.item_unit(id),
    kind         text NOT NULL CHECK (kind IN ('purchase','consignment')),
    pack_price   numeric(15,2) NOT NULL CHECK (pack_price > 0),
    valid_from   date NOT NULL,
    valid_until  date CHECK (valid_until IS NULL OR valid_until >= valid_from),
    note         text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    CONSTRAINT uq_contract_price UNIQUE (supplier_id, item_id, item_unit_id, kind, valid_from)
);
ALTER TABLE inventory.contract_price ENABLE ROW LEVEL SECURITY;
CREATE POLICY contract_price_isolation ON inventory.contract_price
    USING (company_id = current_setting('app.current_company_id')::uuid);

CREATE TABLE inventory.purchase_request (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    location_id      uuid NOT NULL REFERENCES core.location(id),
    pr_no            text NOT NULL,
    status           text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','submitted','approved','rejected','cancelled','converted')),
    needed_on        date,
    note             text,
    submitted_at     timestamptz,
    submitted_by     uuid,
    decided_at        timestamptz,
    decided_by       uuid,
    rejection_reason text,
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid,
    row_version      integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_pr_rejection CHECK (status <> 'rejected' OR btrim(coalesce(rejection_reason, '')) <> ''),
    CONSTRAINT uq_purchase_request_no UNIQUE (merchant_id, pr_no)
);
CREATE TRIGGER trg_purchase_request_touch BEFORE UPDATE ON inventory.purchase_request FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.purchase_request ENABLE ROW LEVEL SECURITY;
CREATE POLICY purchase_request_isolation ON inventory.purchase_request USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_purchase_request_tenant_check BEFORE INSERT OR UPDATE ON inventory.purchase_request FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.purchase_request_line (
    id                  uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id          uuid NOT NULL REFERENCES core.company(id),
    merchant_id         uuid NOT NULL REFERENCES core.merchant(id),
    request_id          uuid NOT NULL REFERENCES inventory.purchase_request(id) ON DELETE CASCADE,
    item_id             uuid NOT NULL REFERENCES inventory.item(id),
    item_unit_id        uuid NOT NULL REFERENCES inventory.item_unit(id),
    pack_qty            numeric(12,3) NOT NULL CHECK (pack_qty > 0),
    note                text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    created_by          uuid
);
CREATE INDEX idx_purchase_request_line_request ON inventory.purchase_request_line (request_id);
ALTER TABLE inventory.purchase_request_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY purchase_request_line_isolation ON inventory.purchase_request_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_purchase_request_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.purchase_request_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.purchase_order (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    supplier_id   uuid NOT NULL REFERENCES core.supplier(id),
    location_id   uuid NOT NULL REFERENCES core.location(id),
    request_id    uuid REFERENCES inventory.purchase_request(id),
    po_no         text NOT NULL,
    status        text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','sent','partial','received','closed','cancelled')),
    note          text,
    sent_at       timestamptz,
    sent_by       uuid,
    closed_at     timestamptz,
    cancel_reason text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_po_cancel CHECK (status <> 'cancelled' OR btrim(coalesce(cancel_reason, '')) <> ''),
    CONSTRAINT uq_purchase_order_no UNIQUE (merchant_id, po_no)
);
CREATE TRIGGER trg_purchase_order_touch BEFORE UPDATE ON inventory.purchase_order FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.purchase_order ENABLE ROW LEVEL SECURITY;
CREATE POLICY purchase_order_isolation ON inventory.purchase_order USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_purchase_order_tenant_check BEFORE INSERT OR UPDATE ON inventory.purchase_order FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.purchase_order_line (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    order_id          uuid NOT NULL REFERENCES inventory.purchase_order(id) ON DELETE CASCADE,
    item_id           uuid NOT NULL REFERENCES inventory.item(id),
    item_unit_id      uuid NOT NULL REFERENCES inventory.item_unit(id),
    pack_qty          numeric(12,3) NOT NULL CHECK (pack_qty > 0),
    pack_content      numeric(12,3) NOT NULL CHECK (pack_content > 0),
    pack_price        numeric(15,2) NOT NULL DEFAULT 0 CHECK (pack_price >= 0),
    received_base_qty numeric(14,3) NOT NULL DEFAULT 0 CHECK (received_base_qty >= 0),
    note              text,
    created_at        timestamptz NOT NULL DEFAULT now(),
    created_by        uuid
);
CREATE INDEX idx_purchase_order_line_order ON inventory.purchase_order_line (order_id);
ALTER TABLE inventory.purchase_order_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY purchase_order_line_isolation ON inventory.purchase_order_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_purchase_order_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.purchase_order_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

ALTER TABLE inventory.goods_receipt ADD COLUMN purchase_order_id uuid REFERENCES inventory.purchase_order(id);
ALTER TABLE inventory.goods_receipt_line ADD COLUMN purchase_order_line_id uuid REFERENCES inventory.purchase_order_line(id);
CREATE INDEX idx_goods_receipt_po ON inventory.goods_receipt (purchase_order_id) WHERE purchase_order_id IS NOT NULL;

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000042', 'inventory.purchase.request', 'Mengusulkan pembelian barang (PR)', 'inventory'),
    ('00000000-0000-0000-0004-000000000043', 'inventory.purchase.approve', 'Menyetujui atau menolak usulan pembelian', 'inventory'),
    ('00000000-0000-0000-0004-000000000044', 'inventory.purchase.manage', 'Mengelola PO, kontrak harga, dan pengaturan pengadaan', 'inventory')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('inventory.purchase.request', 'inventory.purchase.approve', 'inventory.purchase.manage')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('inventory.purchase.request', 'inventory.purchase.approve', 'inventory.purchase.manage')
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
