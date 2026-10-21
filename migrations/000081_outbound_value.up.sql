-- nexqia-api/migrations/000081_outbound_value.up.sql
-- Spec 2026-10-20-inventory-17b-procurement-special-cases §5.1 (P2).

ALTER TABLE inventory.stock_move DROP CONSTRAINT stock_move_move_type_check;
ALTER TABLE inventory.stock_move ADD CONSTRAINT stock_move_move_type_check
    CHECK (move_type IN ('receipt','issue','transfer_out','transfer_in','adjustment','count','status_change','reversal','supplier_return','destruction'));

CREATE TABLE inventory.supplier_return (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    supplier_id  uuid NOT NULL REFERENCES core.supplier(id),
    return_no    text NOT NULL,
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','posted','void')),
    reason       text NOT NULL CHECK (btrim(reason) <> ''),
    note         text,
    posted_at    timestamptz,
    posted_by    uuid,
    void_reason  text,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_supplier_return_posted CHECK ((status = 'draft') = (posted_at IS NULL)),
    CONSTRAINT chk_supplier_return_void CHECK ((status = 'void') = (btrim(coalesce(void_reason, '')) <> '')),
    CONSTRAINT uq_supplier_return_no UNIQUE (merchant_id, return_no)
);
CREATE TRIGGER trg_supplier_return_touch BEFORE UPDATE ON inventory.supplier_return FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.supplier_return ENABLE ROW LEVEL SECURITY;
CREATE POLICY supplier_return_isolation ON inventory.supplier_return USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_supplier_return_tenant_check BEFORE INSERT OR UPDATE ON inventory.supplier_return FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.supplier_return_line (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    return_id   uuid NOT NULL REFERENCES inventory.supplier_return(id) ON DELETE CASCADE,
    item_id     uuid NOT NULL REFERENCES inventory.item(id),
    lot_id      uuid NOT NULL REFERENCES inventory.lot(id),
    source      text NOT NULL CHECK (source IN ('purchase','consignment')),
    base_qty    numeric(14,3) NOT NULL CHECK (base_qty > 0),
    unit_cost   numeric(19,6) NOT NULL DEFAULT 0 CHECK (unit_cost >= 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid
);
CREATE INDEX idx_supplier_return_line_return ON inventory.supplier_return_line (return_id);
ALTER TABLE inventory.supplier_return_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY supplier_return_line_isolation ON inventory.supplier_return_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_supplier_return_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.supplier_return_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.destruction (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    location_id   uuid NOT NULL REFERENCES core.location(id),
    occurred_on   date NOT NULL,
    destruction_no text NOT NULL,
    method        text NOT NULL CHECK (btrim(method) <> ''),
    witnesses     jsonb NOT NULL DEFAULT '[]',
    status        text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','posted','void')),
    note          text,
    posted_at     timestamptz,
    posted_by     uuid,
    void_reason   text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_destruction_posted CHECK ((status = 'draft') = (posted_at IS NULL)),
    CONSTRAINT chk_destruction_void CHECK ((status = 'void') = (btrim(coalesce(void_reason, '')) <> '')),
    CONSTRAINT chk_destruction_witnesses CHECK (jsonb_typeof(witnesses) = 'array'),
    CONSTRAINT uq_destruction_no UNIQUE (merchant_id, destruction_no)
);
CREATE TRIGGER trg_destruction_touch BEFORE UPDATE ON inventory.destruction FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.destruction ENABLE ROW LEVEL SECURITY;
CREATE POLICY destruction_isolation ON inventory.destruction USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_destruction_tenant_check BEFORE INSERT OR UPDATE ON inventory.destruction FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.destruction_line (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    destruction_id uuid NOT NULL REFERENCES inventory.destruction(id) ON DELETE CASCADE,
    item_id     uuid NOT NULL REFERENCES inventory.item(id),
    lot_id      uuid NOT NULL REFERENCES inventory.lot(id),
    source      text NOT NULL CHECK (source IN ('purchase','program','grant','consignment')),
    from_status text NOT NULL CHECK (from_status IN ('available','quarantine','damaged')),
    base_qty    numeric(14,3) NOT NULL CHECK (base_qty > 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid
);
CREATE INDEX idx_destruction_line_destruction ON inventory.destruction_line (destruction_id);
ALTER TABLE inventory.destruction_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY destruction_line_isolation ON inventory.destruction_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_destruction_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.destruction_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.inter_merchant_transfer (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    from_merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    to_merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    from_location_id uuid NOT NULL REFERENCES core.location(id),
    to_location_id   uuid NOT NULL REFERENCES core.location(id),
    imt_no           text NOT NULL,
    status           text NOT NULL DEFAULT 'sent' CHECK (status IN ('sent','received','cancelled')),
    note             text,
    sent_at          timestamptz NOT NULL DEFAULT now(),
    sent_by          uuid,
    received_at      timestamptz,
    received_by      uuid,
    received_on      date,
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid,
    row_version      integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_imt_distinct CHECK (from_merchant_id <> to_merchant_id),
    CONSTRAINT chk_imt_received CHECK ((status = 'received') = (received_at IS NOT NULL AND received_by IS NOT NULL AND received_on IS NOT NULL)),
    CONSTRAINT uq_inter_merchant_transfer_no UNIQUE (company_id, imt_no)
);
CREATE TRIGGER trg_inter_merchant_transfer_touch BEFORE UPDATE ON inventory.inter_merchant_transfer FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.inter_merchant_transfer ENABLE ROW LEVEL SECURITY;
CREATE POLICY inter_merchant_transfer_isolation ON inventory.inter_merchant_transfer
    USING (company_id = current_setting('app.current_company_id')::uuid);

CREATE TABLE inventory.inter_merchant_transfer_line (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    inter_transfer_id uuid NOT NULL REFERENCES inventory.inter_merchant_transfer(id) ON DELETE CASCADE,
    item_id          uuid NOT NULL REFERENCES inventory.item(id),
    lot_id           uuid NOT NULL REFERENCES inventory.lot(id),
    source           text NOT NULL CHECK (source IN ('purchase','program','grant','consignment')),
    status           text NOT NULL CHECK (status IN ('available','quarantine','damaged')),
    base_qty         numeric(14,3) NOT NULL CHECK (base_qty > 0),
    unit_cost        numeric(19,6) NOT NULL DEFAULT 0 CHECK (unit_cost >= 0),
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid
);
CREATE INDEX idx_imt_line_transfer ON inventory.inter_merchant_transfer_line (inter_transfer_id);
ALTER TABLE inventory.inter_merchant_transfer_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY imt_line_isolation ON inventory.inter_merchant_transfer_line
    USING (company_id = current_setting('app.current_company_id')::uuid);

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000045', 'inventory.stock.destroy', 'Memusnahkan stok dan menandatangani berita acara', 'inventory'),
    ('00000000-0000-0000-0004-000000000046', 'inventory.stock.return', 'Mengembalikan barang ke supplier (retur)', 'inventory')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('inventory.stock.destroy', 'inventory.stock.return')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('inventory.stock.destroy', 'inventory.stock.return')
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
