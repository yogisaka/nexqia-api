-- nexqia-api/migrations/000082_identity_packaging.up.sql
-- Spec 2026-10-20-inventory-17b-procurement-special-cases §6.1 (P3).
-- Down = degradasi terdokumentasi: baris konsinyasi beda supplier menyatu (spec §6.1).

ALTER TABLE inventory.item ADD COLUMN track_serial boolean NOT NULL DEFAULT false;

ALTER TABLE inventory.stock_move DROP CONSTRAINT stock_move_move_type_check;
ALTER TABLE inventory.stock_move ADD CONSTRAINT stock_move_move_type_check
    CHECK (move_type IN ('receipt','issue','transfer_out','transfer_in','adjustment','count','status_change','reversal','supplier_return','destruction','repack_out','repack_in'));

ALTER TABLE inventory.stock_balance ADD COLUMN supplier_id uuid REFERENCES core.supplier(id);
ALTER TABLE inventory.stock_balance ADD CONSTRAINT chk_stock_balance_supplier
    CHECK (supplier_id IS NULL OR source = 'consignment') NOT VALID;
ALTER TABLE inventory.stock_balance DROP CONSTRAINT stock_balance_location_id_lot_id_source_status_key;
CREATE UNIQUE INDEX uq_stock_balance_slot ON inventory.stock_balance
    (location_id, lot_id, source, status, COALESCE(supplier_id, '00000000-0000-0000-0000-000000000000'));

CREATE TABLE inventory.consignment_usage (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    supplier_id uuid NOT NULL REFERENCES core.supplier(id),
    item_id     uuid NOT NULL REFERENCES inventory.item(id),
    lot_id      uuid NOT NULL REFERENCES inventory.lot(id),
    base_qty    numeric(14,3) NOT NULL CHECK (base_qty > 0),
    unit_price  numeric(19,6) NOT NULL CHECK (unit_price >= 0),
    used_on     date NOT NULL,
    ref_type    text,
    ref_id      uuid,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid
);
CREATE INDEX idx_consignment_usage_report ON inventory.consignment_usage (merchant_id, supplier_id, item_id, used_on);
ALTER TABLE inventory.consignment_usage ENABLE ROW LEVEL SECURITY;
CREATE POLICY consignment_usage_isolation ON inventory.consignment_usage USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_consignment_usage_tenant_check BEFORE INSERT OR UPDATE ON inventory.consignment_usage FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON inventory.consignment_usage FROM app_runtime;

CREATE TABLE inventory.serial (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    item_id         uuid NOT NULL REFERENCES inventory.item(id),
    serial_no       text NOT NULL CHECK (btrim(serial_no) <> ''),
    lot_id          uuid REFERENCES inventory.lot(id),
    location_id     uuid NOT NULL REFERENCES core.location(id),
    status          text NOT NULL DEFAULT 'in_stock' CHECK (status IN ('in_stock','issued','destroyed','returned')),
    issued_ref_type text,
    issued_ref_id   uuid,
    received_at     timestamptz NOT NULL DEFAULT now(),
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    CONSTRAINT uq_serial UNIQUE (merchant_id, item_id, serial_no)
);
CREATE INDEX idx_serial_lookup ON inventory.serial (merchant_id, item_id, status);
ALTER TABLE inventory.serial ENABLE ROW LEVEL SECURITY;
CREATE POLICY serial_isolation ON inventory.serial USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_serial_tenant_check BEFORE INSERT OR UPDATE ON inventory.serial FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.repack (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id     uuid NOT NULL REFERENCES core.company(id),
    merchant_id    uuid NOT NULL REFERENCES core.merchant(id),
    out_location_id uuid NOT NULL REFERENCES core.location(id),
    in_location_id uuid NOT NULL REFERENCES core.location(id),
    status         text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','posted','void')),
    note           text,
    posted_at      timestamptz,
    posted_by      uuid,
    void_reason    text,
    created_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     uuid,
    row_version    integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_repack_posted CHECK ((status = 'draft') = (posted_at IS NULL)),
    CONSTRAINT chk_repack_void CHECK ((status = 'void') = (btrim(coalesce(void_reason, '')) <> '')),
    CONSTRAINT chk_repack_locations CHECK (out_location_id <> in_location_id)
);
CREATE TRIGGER trg_repack_touch BEFORE UPDATE ON inventory.repack FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.repack ENABLE ROW LEVEL SECURITY;
CREATE POLICY repack_isolation ON inventory.repack USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_repack_tenant_check BEFORE INSERT OR UPDATE ON inventory.repack FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.repack_out_line (
    id         uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    repack_id  uuid NOT NULL REFERENCES inventory.repack(id) ON DELETE CASCADE,
    item_id    uuid NOT NULL REFERENCES inventory.item(id),
    lot_id     uuid NOT NULL REFERENCES inventory.lot(id),
    source     text NOT NULL CHECK (source IN ('purchase','program','grant','consignment')),
    base_qty   numeric(14,3) NOT NULL CHECK (base_qty > 0),
    created_at timestamptz NOT NULL DEFAULT now(),
    created_by uuid
);
CREATE INDEX idx_repack_out_line_repack ON inventory.repack_out_line (repack_id);
ALTER TABLE inventory.repack_out_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY repack_out_line_isolation ON inventory.repack_out_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_repack_out_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.repack_out_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.repack_in_line (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    repack_id    uuid NOT NULL REFERENCES inventory.repack(id) ON DELETE CASCADE,
    item_id      uuid NOT NULL REFERENCES inventory.item(id),
    item_unit_id uuid NOT NULL REFERENCES inventory.item_unit(id),
    pack_qty     numeric(12,3) NOT NULL CHECK (pack_qty > 0),
    pack_content numeric(12,3) NOT NULL CHECK (pack_content > 0),
    batch_no     text,
    expiry_date  date,
    base_qty     numeric(14,3) NOT NULL CHECK (base_qty > 0),
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid
);
CREATE INDEX idx_repack_in_line_repack ON inventory.repack_in_line (repack_id);
ALTER TABLE inventory.repack_in_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY repack_in_line_isolation ON inventory.repack_in_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_repack_in_line_tenant_check BEFORE INSERT OR UPDATE ON inventory.repack_in_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
