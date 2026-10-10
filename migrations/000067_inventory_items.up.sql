-- nexqia-api/migrations/000067_inventory_items.up.sql
-- Spec 2026-10-07-tariff-c-drug-master-pricing §3: unit dictionary, company-level
-- item master (KFA, regulatory, packaging, compounding, BUD), per-merchant
-- formulary linked to core.service_item, margin rules and price proposals.

CREATE SCHEMA inventory;
GRANT USAGE ON SCHEMA inventory TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA inventory GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA inventory GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;

-- §3.2 platform unit dictionary (read-only for tenants)
CREATE TABLE inventory.unit (
    id         uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    code       text NOT NULL UNIQUE,
    name       text NOT NULL,
    ucum_code  text,
    sort_order integer NOT NULL
);
INSERT INTO inventory.unit (code, name, ucum_code, sort_order) VALUES
    ('tablet', 'Tablet', '{tbl}', 1),
    ('kaplet', 'Kaplet', '{tbl}', 2),
    ('kapsul', 'Kapsul', '{cap}', 3),
    ('tablet_salut', 'Tablet Salut', '{tbl}', 4),
    ('ml', 'mL', 'mL', 5),
    ('l', 'Liter', 'L', 6),
    ('g', 'Gram', 'g', 7),
    ('kg', 'Kilogram', 'kg', 8),
    ('mg', 'Miligram', 'mg', 9),
    ('pcs', 'Pcs', '{pcs}', 10),
    ('vial', 'Vial', '{vial}', 11),
    ('ampul', 'Ampul', '{amp}', 12),
    ('botol', 'Botol', '{btl}', 13),
    ('tube', 'Tube', '{tube}', 14),
    ('pot', 'Pot', '{pot}', 15),
    ('sachet', 'Sachet', '{sachet}', 16),
    ('strip', 'Strip', '{strip}', 17),
    ('blister', 'Blister', '{blister}', 18),
    ('box', 'Box', '{box}', 19),
    ('dus', 'Dus', '{dus}', 20),
    ('flakon', 'Flakon', '{flakon}', 21),
    ('pen', 'Pen', '{pen}', 22),
    ('set', 'Set', '{set}', 23),
    ('roll', 'Roll', '{roll}', 24),
    ('lembar', 'Lembar', '{lembar}', 25),
    ('pasang', 'Pasang', '{pasang}', 26),
    ('supositoria', 'Supositoria', '{supp}', 27),
    ('ovula', 'Ovula', '{ovula}', 28),
    ('kantong', 'Kantong', '{kantong}', 29),
    ('galon', 'Galon', '{galon}', 30);
REVOKE INSERT, UPDATE, DELETE ON inventory.unit FROM app_runtime;

-- §3.3 company-level item master
CREATE TABLE inventory.item (
    id                             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id                     uuid NOT NULL REFERENCES core.company(id),
    code                           text NOT NULL CHECK (btrim(code) <> ''),
    name                           text NOT NULL CHECK (btrim(name) <> ''),
    item_kind                      text NOT NULL CHECK (item_kind IN ('drug','vaccine','medical_device','material','reagent','general')),
    pricing_category               text NOT NULL CHECK (pricing_category IN ('generic','branded','otc','bmhp','medical_device','oncology','vaccine','other')),
    base_unit_id                   uuid NOT NULL REFERENCES inventory.unit(id),
    kfa_code                       text CHECK (kfa_code ~ '^93[0-9]{6,}$'),
    kfa_pov_code                   text CHECK (kfa_pov_code ~ '^92[0-9]{6,}$'),
    non_kfa                        boolean NOT NULL DEFAULT false,
    non_kfa_reason                 text,
    nie                            text,
    manufacturer                   text,
    dosage_form                    text,
    drug_class                     text CHECK (drug_class IN ('bebas','bebas_terbatas','keras','narkotika','psikotropika','prekursor')),
    is_oot                         boolean NOT NULL DEFAULT false,
    is_fornas                      boolean NOT NULL DEFAULT false,
    fornas_period                  text,
    is_high_alert                  boolean NOT NULL DEFAULT false,
    is_lasa                        boolean NOT NULL DEFAULT false,
    lasa_note                      text,
    is_generic                     boolean NOT NULL DEFAULT false,
    het_amount                     numeric(15,2) CHECK (het_amount > 0),
    het_effective_from             date,
    min_fraction                   numeric(3,2) NOT NULL DEFAULT 1 CHECK (min_fraction IN (1, 0.5, 0.25)),
    no_crush                       boolean NOT NULL DEFAULT false,
    bud_after_open_hours           integer CHECK (bud_after_open_hours > 0),
    bud_after_reconstitution_hours integer CHECK (bud_after_reconstitution_hours > 0),
    is_single_dose                 boolean NOT NULL DEFAULT false,
    storage_temp                   text NOT NULL DEFAULT 'room' CHECK (storage_temp IN ('room','cool','cold','frozen')),
    track_batch                    boolean NOT NULL DEFAULT false,
    track_expiry                   boolean NOT NULL DEFAULT false,
    is_active                      boolean NOT NULL DEFAULT true,
    created_at                     timestamptz NOT NULL DEFAULT now(),
    created_by                     uuid,
    updated_at                     timestamptz NOT NULL DEFAULT now(),
    updated_by                     uuid,
    deleted_at                     timestamptz,
    deleted_by                     uuid,
    row_version                    integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_item_kfa CHECK (item_kind NOT IN ('drug','vaccine','medical_device')
        OR kfa_code IS NOT NULL OR (non_kfa AND btrim(coalesce(non_kfa_reason, '')) <> '')),
    CONSTRAINT chk_item_drug_class CHECK ((item_kind IN ('drug','vaccine')) = (drug_class IS NOT NULL)),
    CONSTRAINT chk_item_het_pair CHECK ((het_amount IS NULL) = (het_effective_from IS NULL)),
    CONSTRAINT chk_item_lasa CHECK (NOT is_lasa OR btrim(coalesce(lasa_note, '')) <> ''),
    CONSTRAINT chk_item_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_item_company_code ON inventory.item (company_id, code) WHERE deleted_at IS NULL;
CREATE INDEX idx_item_company ON inventory.item (company_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_item_name_trgm ON inventory.item USING gin (name gin_trgm_ops) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_item_touch BEFORE UPDATE ON inventory.item
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.item ENABLE ROW LEVEL SECURITY;
CREATE POLICY item_isolation ON inventory.item
    USING (company_id = current_setting('app.current_company_id')::uuid);

-- §3.4 active ingredients (BZA) per item
CREATE TABLE inventory.item_ingredient (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    item_id         uuid NOT NULL REFERENCES inventory.item(id) ON DELETE CASCADE,
    ingredient_code text CHECK (ingredient_code ~ '^91[0-9]{6,}$'),
    ingredient_name text NOT NULL CHECK (btrim(ingredient_name) <> ''),
    strength        numeric(14,4) CHECK (strength > 0),
    strength_unit   text,
    per_quantity    numeric(12,3) NOT NULL DEFAULT 1 CHECK (per_quantity > 0),
    per_unit        text,
    sort_order      integer NOT NULL DEFAULT 0,
    CONSTRAINT chk_item_ingredient_strength CHECK ((strength IS NULL) = (strength_unit IS NULL))
);
CREATE INDEX idx_item_ingredient_item ON inventory.item_ingredient (item_id);
CREATE INDEX idx_item_ingredient_code ON inventory.item_ingredient (company_id, ingredient_code);
ALTER TABLE inventory.item_ingredient ENABLE ROW LEVEL SECURITY;
CREATE POLICY item_ingredient_isolation ON inventory.item_ingredient
    USING (company_id = current_setting('app.current_company_id')::uuid);

-- §3.5 packaging levels; factor = base units per one package (C3)
CREATE TABLE inventory.item_unit (
    id                  uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id          uuid NOT NULL REFERENCES core.company(id),
    item_id             uuid NOT NULL REFERENCES inventory.item(id),
    unit_id             uuid NOT NULL REFERENCES inventory.unit(id),
    factor              numeric(12,3) NOT NULL CHECK (factor > 0),
    kfa_packaging_code  text,
    is_default_purchase boolean NOT NULL DEFAULT false,
    is_default_issue    boolean NOT NULL DEFAULT false,
    is_active           boolean NOT NULL DEFAULT true,
    created_at          timestamptz NOT NULL DEFAULT now(),
    created_by          uuid
);
CREATE UNIQUE INDEX uq_item_unit_active ON inventory.item_unit (item_id, unit_id) WHERE is_active;
CREATE UNIQUE INDEX uq_item_unit_default_purchase ON inventory.item_unit (item_id) WHERE is_active AND is_default_purchase;
CREATE UNIQUE INDEX uq_item_unit_default_issue ON inventory.item_unit (item_id) WHERE is_active AND is_default_issue;
ALTER TABLE inventory.item_unit ENABLE ROW LEVEL SECURITY;
CREATE POLICY item_unit_isolation ON inventory.item_unit
    USING (company_id = current_setting('app.current_company_id')::uuid);

CREATE FUNCTION inventory.trg_item_unit_guard() RETURNS trigger LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.item_id <> OLD.item_id OR NEW.unit_id <> OLD.unit_id OR NEW.factor <> OLD.factor THEN
        RAISE EXCEPTION 'packaging factor cannot be changed' USING ERRCODE = 'check_violation';
    END IF;
    IF OLD.is_active AND NOT NEW.is_active AND EXISTS (
        SELECT 1 FROM inventory.item i WHERE i.id = NEW.item_id AND i.base_unit_id = NEW.unit_id) THEN
        RAISE EXCEPTION 'base unit packaging cannot be deactivated' USING ERRCODE = 'check_violation';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_item_unit_guard BEFORE UPDATE ON inventory.item_unit
    FOR EACH ROW EXECUTE FUNCTION inventory.trg_item_unit_guard();

-- §3.6 per-merchant formulary (C1)
CREATE TABLE inventory.merchant_item (
    id                     uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id             uuid NOT NULL REFERENCES core.company(id),
    merchant_id            uuid NOT NULL REFERENCES core.merchant(id),
    item_id                uuid NOT NULL REFERENCES inventory.item(id),
    service_item_id        uuid NOT NULL UNIQUE REFERENCES core.service_item(id),
    default_location_id    uuid REFERENCES core.location(id),
    is_active              boolean NOT NULL DEFAULT true,
    reference_amount       numeric(15,2) CHECK (reference_amount > 0),
    reference_item_unit_id uuid REFERENCES inventory.item_unit(id),
    reference_factor       numeric(12,3) CHECK (reference_factor > 0),
    reference_updated_at   timestamptz,
    reference_updated_by   uuid,
    created_at             timestamptz NOT NULL DEFAULT now(),
    created_by             uuid,
    updated_at             timestamptz NOT NULL DEFAULT now(),
    updated_by             uuid,
    deleted_at             timestamptz,
    deleted_by             uuid,
    row_version            integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_merchant_item_reference CHECK (
        (reference_amount IS NULL) = (reference_item_unit_id IS NULL)
        AND (reference_amount IS NULL) = (reference_factor IS NULL)
        AND (reference_amount IS NULL) = (reference_updated_at IS NULL)),
    CONSTRAINT chk_merchant_item_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_merchant_item ON inventory.merchant_item (merchant_id, item_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_merchant_item_item ON inventory.merchant_item (item_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_merchant_item_touch BEFORE UPDATE ON inventory.merchant_item
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE inventory.merchant_item ENABLE ROW LEVEL SECURITY;
CREATE POLICY merchant_item_isolation ON inventory.merchant_item
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_merchant_item_tenant_check BEFORE INSERT OR UPDATE ON inventory.merchant_item
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- Cross-merchant item rules (spec §4.1.2, §4.1.5). SECURITY DEFINER: the
-- formulary rows and service items of OTHER merchants are invisible under RLS.
CREATE FUNCTION inventory.trg_item_guard() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = inventory, core, pg_temp AS $$
BEGIN
    IF NEW.base_unit_id <> OLD.base_unit_id THEN
        RAISE EXCEPTION 'base unit cannot be changed' USING ERRCODE = 'check_violation';
    END IF;
    IF ((OLD.is_active AND NOT NEW.is_active) OR (OLD.deleted_at IS NULL AND NEW.deleted_at IS NOT NULL))
       AND EXISTS (SELECT 1 FROM inventory.merchant_item mi
                    WHERE mi.item_id = NEW.id AND mi.is_active AND mi.deleted_at IS NULL) THEN
        RAISE EXCEPTION 'item is active in a formulary' USING ERRCODE = 'check_violation';
    END IF;
    IF NEW.name IS DISTINCT FROM OLD.name THEN
        UPDATE core.service_item s SET name = NEW.name, updated_by = NEW.updated_by
          FROM inventory.merchant_item mi
         WHERE mi.item_id = NEW.id AND s.id = mi.service_item_id;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_item_guard BEFORE UPDATE ON inventory.item
    FOR EACH ROW EXECUTE FUNCTION inventory.trg_item_guard();

-- §3.7 margin per price list × pricing category (C4)
CREATE TABLE inventory.margin_rule (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    price_list_id    uuid NOT NULL REFERENCES core.price_list(id) ON DELETE CASCADE,
    pricing_category text NOT NULL CHECK (pricing_category IN ('generic','branded','otc','bmhp','medical_device','oncology','vaccine','other')),
    margin_percent   numeric(7,2) NOT NULL CHECK (margin_percent > -100 AND margin_percent <= 1000),
    rounding_unit    numeric(15,2) NOT NULL DEFAULT 0 CHECK (rounding_unit >= 0),
    auto_approve     boolean NOT NULL DEFAULT false,
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    UNIQUE (price_list_id, pricing_category)
);
ALTER TABLE inventory.margin_rule ENABLE ROW LEVEL SECURITY;
CREATE POLICY margin_rule_isolation ON inventory.margin_rule
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_margin_rule_tenant_check BEFORE INSERT OR UPDATE ON inventory.margin_rule
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- §3.8 sell price proposals
CREATE TABLE inventory.price_proposal (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    merchant_item_id uuid NOT NULL REFERENCES inventory.merchant_item(id),
    price_list_id    uuid NOT NULL REFERENCES core.price_list(id) ON DELETE CASCADE,
    reason           text NOT NULL CHECK (reason IN ('reference_changed','margin_changed','het_changed')),
    current_amount   numeric(15,2),
    proposed_amount  numeric(15,2) NOT NULL CHECK (proposed_amount > 0),
    het_capped       boolean NOT NULL DEFAULT false,
    status           text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending','approved','rejected','superseded','auto_applied')),
    effective_from   date,
    decided_by       uuid,
    decided_at       timestamptz,
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    CONSTRAINT chk_price_proposal_applied CHECK ((status IN ('approved','auto_applied')) = (effective_from IS NOT NULL))
);
CREATE UNIQUE INDEX uq_price_proposal_pending ON inventory.price_proposal (merchant_item_id, price_list_id) WHERE status = 'pending';
CREATE INDEX idx_price_proposal_merchant ON inventory.price_proposal (merchant_id, status, created_at DESC);
ALTER TABLE inventory.price_proposal ENABLE ROW LEVEL SECURITY;
CREATE POLICY price_proposal_isolation ON inventory.price_proposal
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_price_proposal_tenant_check BEFORE INSERT OR UPDATE ON inventory.price_proposal
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- §3.9 permission (id pattern of 000043; 032–034 taken by 000065)
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000035', 'inventory.item.manage', 'Kelola master barang, formularium, dan harga referensi', 'inventory')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id
FROM core.role r JOIN core.permission p ON p.code = 'inventory.item.manage'
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'inventory.item.manage'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
