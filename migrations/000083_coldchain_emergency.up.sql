-- nexqia-api/migrations/000083_coldchain_emergency.up.sql
-- Spec 2026-10-20-inventory-17b-procurement-special-cases §7.1 (P4).

CREATE TABLE inventory.opened_container (
    id                 uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id         uuid NOT NULL REFERENCES core.company(id),
    merchant_id        uuid NOT NULL REFERENCES core.merchant(id),
    item_id            uuid NOT NULL REFERENCES inventory.item(id),
    lot_id             uuid NOT NULL REFERENCES inventory.lot(id),
    location_id        uuid NOT NULL REFERENCES core.location(id),
    opened_at          timestamptz NOT NULL,
    reconstituted      boolean NOT NULL DEFAULT false,
    bud_at             timestamptz NOT NULL,
    remaining_base_qty numeric(14,3) NOT NULL CHECK (remaining_base_qty >= 0),
    status             text NOT NULL DEFAULT 'open' CHECK (status IN ('open','finished','expired','wasted')),
    note               text,
    created_at         timestamptz NOT NULL DEFAULT now(),
    created_by         uuid,
    updated_at         timestamptz NOT NULL DEFAULT now(),
    updated_by         uuid
);
CREATE INDEX idx_opened_container_lookup ON inventory.opened_container (merchant_id, status);
ALTER TABLE inventory.opened_container ENABLE ROW LEVEL SECURITY;
CREATE POLICY opened_container_isolation ON inventory.opened_container USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_opened_container_tenant_check BEFORE INSERT OR UPDATE ON inventory.opened_container FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.vvm_log (
    id                  uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id          uuid NOT NULL REFERENCES core.company(id),
    merchant_id         uuid NOT NULL REFERENCES core.merchant(id),
    opened_container_id uuid NOT NULL REFERENCES inventory.opened_container(id),
    checked_at          timestamptz NOT NULL,
    stage               smallint NOT NULL CHECK (stage BETWEEN 1 AND 4),
    checked_by          uuid,
    created_at          timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_vvm_log_container ON inventory.vvm_log (opened_container_id);
ALTER TABLE inventory.vvm_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY vvm_log_isolation ON inventory.vvm_log USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_vvm_log_tenant_check BEFORE INSERT OR UPDATE ON inventory.vvm_log FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON inventory.vvm_log FROM app_runtime;

CREATE TABLE inventory.fridge (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    name        text NOT NULL CHECK (btrim(name) <> ''),
    location_id uuid REFERENCES core.location(id),
    note        text,
    is_active   boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid
);
ALTER TABLE inventory.fridge ENABLE ROW LEVEL SECURITY;
CREATE POLICY fridge_isolation ON inventory.fridge USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_fridge_tenant_check BEFORE INSERT OR UPDATE ON inventory.fridge FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.fridge_temp_log (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    fridge_id   uuid NOT NULL REFERENCES inventory.fridge(id),
    logged_at   timestamptz NOT NULL,
    temperature numeric(4,1) NOT NULL,
    taken_by    uuid,
    note        text,
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_fridge_temp_log_report ON inventory.fridge_temp_log (merchant_id, fridge_id, logged_at);
ALTER TABLE inventory.fridge_temp_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY fridge_temp_log_isolation ON inventory.fridge_temp_log USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_fridge_temp_log_tenant_check BEFORE INSERT OR UPDATE ON inventory.fridge_temp_log FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON inventory.fridge_temp_log FROM app_runtime;

CREATE TABLE inventory.emergency_kit (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    name        text NOT NULL CHECK (btrim(name) <> ''),
    location_id uuid NOT NULL REFERENCES core.location(id),
    note        text,
    is_active   boolean NOT NULL DEFAULT true,
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    updated_at  timestamptz NOT NULL DEFAULT now(),
    updated_by  uuid
);
ALTER TABLE inventory.emergency_kit ENABLE ROW LEVEL SECURITY;
CREATE POLICY emergency_kit_isolation ON inventory.emergency_kit USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_emergency_kit_tenant_check BEFORE INSERT OR UPDATE ON inventory.emergency_kit FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.emergency_kit_item (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    kit_id      uuid NOT NULL REFERENCES inventory.emergency_kit(id) ON DELETE CASCADE,
    item_id     uuid NOT NULL REFERENCES inventory.item(id),
    base_qty    numeric(14,3) NOT NULL CHECK (base_qty > 0),
    created_at  timestamptz NOT NULL DEFAULT now(),
    created_by  uuid,
    CONSTRAINT uq_emergency_kit_item UNIQUE (kit_id, item_id)
);
ALTER TABLE inventory.emergency_kit_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY emergency_kit_item_isolation ON inventory.emergency_kit_item USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_emergency_kit_item_tenant_check BEFORE INSERT OR UPDATE ON inventory.emergency_kit_item FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE inventory.emergency_kit_seal (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    kit_id      uuid NOT NULL REFERENCES inventory.emergency_kit(id),
    seal_no     text NOT NULL CHECK (btrim(seal_no) <> ''),
    sealed_at   timestamptz NOT NULL DEFAULT now(),
    sealed_by   uuid,
    broken_at   timestamptz,
    broken_by   uuid,
    note        text
);
CREATE UNIQUE INDEX uq_emergency_kit_seal_active ON inventory.emergency_kit_seal (kit_id) WHERE broken_at IS NULL;
ALTER TABLE inventory.emergency_kit_seal ENABLE ROW LEVEL SECURITY;
CREATE POLICY emergency_kit_seal_isolation ON inventory.emergency_kit_seal USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_emergency_kit_seal_tenant_check BEFORE INSERT OR UPDATE ON inventory.emergency_kit_seal FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000047', 'inventory.coldchain.log', 'Mencatat suhu lemari vaksin, VVM, dan kemasan terbuka (BUD)', 'inventory'),
    ('00000000-0000-0000-0004-000000000048', 'inventory.emergency.use', 'Membuka segel dan memakai troli emergensi', 'inventory')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('inventory.coldchain.log', 'inventory.emergency.use')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('inventory.coldchain.log', 'inventory.emergency.use')
WHERE tr.name IN ('Admin Klinik', 'Admin RS', 'Perawat')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
