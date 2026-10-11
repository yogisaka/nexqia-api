-- nexqia-api/migrations/000070_practitioner_fees_tax.up.sql
-- Spec 2026-10-07-tariff-d-practitioner-fees-tax §3. Tax reference data:
-- TER = Lampiran PP 58/2023; PTKP = PMK 101/PMK.010/2016; Pasal 17 = UU 7/2021 (HPP);
-- biaya jabatan = PMK 250/PMK.03/2008. All effective 2024-01-01.

ALTER TABLE core.physician
    ADD COLUMN profession text NOT NULL DEFAULT 'doctor' CHECK (profession IN ('doctor','dentist','midwife','nurse','pharmacist','other_health')),
    ADD COLUMN employment_type text NOT NULL DEFAULT 'partner' CHECK (employment_type IN ('owner','employee','partner')),
    ADD COLUMN ptkp_status text CHECK (ptkp_status IN ('TK/0','TK/1','TK/2','TK/3','K/0','K/1','K/2','K/3')),
    ADD COLUMN npwp text,
    ADD CONSTRAINT chk_physician_ptkp CHECK ((employment_type = 'employee') = (ptkp_status IS NOT NULL));

ALTER TABLE operations.charge_split ADD COLUMN components jsonb
    CHECK (components IS NULL OR jsonb_typeof(components) = 'array');

CREATE TABLE operations.charge_performer (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    charge_id    uuid NOT NULL REFERENCES operations.charge(id) ON DELETE CASCADE,
    physician_id uuid NOT NULL REFERENCES core.physician(id),
    role         text NOT NULL CHECK (role IN ('primary','assistant','anesthesia','midwife','nurse','pharmacist','other')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    UNIQUE (charge_id, physician_id, role)
);
ALTER TABLE operations.charge_performer ENABLE ROW LEVEL SECURITY;
CREATE POLICY charge_performer_isolation ON operations.charge_performer USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_charge_performer_tenant_check BEFORE INSERT OR UPDATE ON operations.charge_performer FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.practitioner_fee_rule (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    physician_id    uuid REFERENCES core.physician(id),
    service_item_id uuid REFERENCES core.service_item(id),
    item_type       text CHECK (item_type IN ('consultation','procedure','administration','room','package','pharmacy_fee','drug','medical_device','material')),
    payer_id        uuid REFERENCES core.payer(id),
    performer_role  text CHECK (performer_role IN ('primary','assistant','anesthesia','midwife','nurse','pharmacist','other')),
    basis           text NOT NULL CHECK (basis IN ('percent_of_component','percent_of_total','fixed_per_item','fixed_per_visit')),
    component_code  text,
    percent         numeric(5,2) CHECK (percent >= 0 AND percent <= 100),
    amount          numeric(15,2) CHECK (amount >= 0),
    effective_from  date NOT NULL,
    effective_to    date CHECK (effective_to IS NULL OR effective_to >= effective_from),
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      uuid,
    deleted_at      timestamptz,
    deleted_by      uuid,
    row_version     integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_fee_rule_target CHECK (service_item_id IS NULL OR item_type IS NULL),
    CONSTRAINT chk_fee_rule_component CHECK ((basis = 'percent_of_component') = (component_code IS NOT NULL)),
    CONSTRAINT chk_fee_rule_value CHECK (
        (basis IN ('percent_of_component','percent_of_total') AND percent IS NOT NULL AND amount IS NULL) OR
        (basis IN ('fixed_per_item','fixed_per_visit') AND amount IS NOT NULL AND percent IS NULL)),
    CONSTRAINT chk_fee_rule_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE INDEX idx_fee_rule_merchant ON core.practitioner_fee_rule (merchant_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_fee_rule_touch BEFORE UPDATE ON core.practitioner_fee_rule FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.practitioner_fee_rule ENABLE ROW LEVEL SECURITY;
CREATE POLICY practitioner_fee_rule_isolation ON core.practitioner_fee_rule USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_fee_rule_tenant_check BEFORE INSERT OR UPDATE ON core.practitioner_fee_rule FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.fee_payout_batch (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    period_year  smallint NOT NULL CHECK (period_year BETWEEN 2024 AND 2100),
    period_month smallint NOT NULL CHECK (period_month BETWEEN 1 AND 12),
    status       text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','locked','paid')),
    locked_at    timestamptz,
    locked_by    uuid,
    paid_at      timestamptz,
    paid_by      uuid,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, period_year, period_month),
    CONSTRAINT chk_fee_batch_locked CHECK ((status = 'draft') = (locked_at IS NULL)),
    CONSTRAINT chk_fee_batch_paid CHECK ((status = 'paid') = (paid_at IS NOT NULL))
);
CREATE TRIGGER trg_fee_batch_touch BEFORE UPDATE ON operations.fee_payout_batch FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.fee_payout_batch ENABLE ROW LEVEL SECURITY;
CREATE POLICY fee_payout_batch_isolation ON operations.fee_payout_batch USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_fee_batch_tenant_check BEFORE INSERT OR UPDATE ON operations.fee_payout_batch FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.practitioner_fee (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    account_id    uuid NOT NULL REFERENCES operations.billing_account(id),
    charge_id     uuid NOT NULL REFERENCES operations.charge(id),
    physician_id  uuid NOT NULL REFERENCES core.physician(id),
    role          text NOT NULL,
    service_date  date NOT NULL,
    gross_amount  numeric(15,2) NOT NULL CHECK (gross_amount >= 0),
    fee_amount    numeric(15,2) NOT NULL CHECK (fee_amount >= 0),
    rule_id       uuid REFERENCES core.practitioner_fee_rule(id),
    rule_snapshot jsonb NOT NULL DEFAULT '{}'::jsonb,
    status        text NOT NULL DEFAULT 'open' CHECK (status IN ('open','in_batch','paid')),
    batch_id      uuid REFERENCES operations.fee_payout_batch(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    UNIQUE (charge_id, physician_id, role),
    CONSTRAINT chk_practitioner_fee_batch CHECK ((status = 'open') = (batch_id IS NULL))
);
CREATE INDEX idx_practitioner_fee_open ON operations.practitioner_fee (merchant_id, physician_id, service_date) WHERE status = 'open';
CREATE INDEX idx_practitioner_fee_account ON operations.practitioner_fee (account_id);
ALTER TABLE operations.practitioner_fee ENABLE ROW LEVEL SECURITY;
CREATE POLICY practitioner_fee_isolation ON operations.practitioner_fee USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_practitioner_fee_tenant_check BEFORE INSERT OR UPDATE ON operations.practitioner_fee FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.fee_payout_line (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    batch_id        uuid NOT NULL REFERENCES operations.fee_payout_batch(id) ON DELETE CASCADE,
    physician_id    uuid NOT NULL REFERENCES core.physician(id),
    employment_type text NOT NULL CHECK (employment_type IN ('owner','employee','partner')),
    ptkp_status     text,
    gross_amount    numeric(15,2) NOT NULL DEFAULT 0,
    fee_amount      numeric(15,2) NOT NULL DEFAULT 0,
    other_gross     numeric(15,2) NOT NULL DEFAULT 0 CHECK (other_gross >= 0),
    tax_method      text CHECK (tax_method IN ('partner_50pct','ter_monthly','annual_december','none')),
    tax_base        numeric(15,2),
    ter_category    text CHECK (ter_category IN ('A','B','C')),
    tax_rate        numeric(5,2),
    tax_amount      numeric(15,2),
    net_amount      numeric(15,2),
    UNIQUE (batch_id, physician_id),
    CONSTRAINT chk_fee_line_other_gross CHECK (employment_type = 'employee' OR other_gross = 0),
    CONSTRAINT chk_fee_line_negative_tax CHECK (tax_amount IS NULL OR tax_amount >= 0 OR tax_method = 'annual_december')
);
ALTER TABLE operations.fee_payout_line ENABLE ROW LEVEL SECURITY;
CREATE POLICY fee_payout_line_isolation ON operations.fee_payout_line USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_fee_line_tenant_check BEFORE INSERT OR UPDATE ON operations.fee_payout_line FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- Platform tax reference data (read-only for tenants).
CREATE TABLE core.tax_ter_rate (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    category        text NOT NULL CHECK (category IN ('A','B','C')),
    lower_exclusive numeric(15,2) NOT NULL CHECK (lower_exclusive >= 0),
    upper_inclusive numeric(15,2) CHECK (upper_inclusive > lower_exclusive),
    rate_percent    numeric(5,2) NOT NULL CHECK (rate_percent >= 0 AND rate_percent <= 100),
    effective_from  date NOT NULL,
    UNIQUE (category, lower_exclusive, effective_from)
);
CREATE TABLE core.tax_ptkp (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    status         text NOT NULL CHECK (status IN ('TK/0','TK/1','TK/2','TK/3','K/0','K/1','K/2','K/3')),
    annual_amount  numeric(15,2) NOT NULL CHECK (annual_amount > 0),
    effective_from date NOT NULL,
    UNIQUE (status, effective_from)
);
CREATE TABLE core.tax_pasal17_bracket (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    upper_inclusive numeric(15,2),
    rate_percent    numeric(5,2) NOT NULL CHECK (rate_percent > 0 AND rate_percent <= 100),
    effective_from  date NOT NULL,
    UNIQUE (upper_inclusive, effective_from)
);
CREATE TABLE core.tax_setting (
    key            text NOT NULL CHECK (key IN ('biaya_jabatan_percent','biaya_jabatan_max_annual','partner_base_percent')),
    value          numeric(15,2) NOT NULL,
    effective_from date NOT NULL,
    PRIMARY KEY (key, effective_from)
);
REVOKE INSERT, UPDATE, DELETE ON core.tax_ter_rate, core.tax_ptkp, core.tax_pasal17_bracket, core.tax_setting FROM app_runtime;

INSERT INTO core.tax_ter_rate (category, lower_exclusive, upper_inclusive, rate_percent, effective_from)
SELECT v.category, v.lower_exclusive, v.upper_inclusive, v.rate_percent, DATE '2024-01-01'
FROM (VALUES
    ('A', 0, 5400000, 0),
    ('A', 5400000, 5650000, 0.25),
    ('A', 5650000, 5950000, 0.5),
    ('A', 5950000, 6300000, 0.75),
    ('A', 6300000, 6750000, 1),
    ('A', 6750000, 7500000, 1.25),
    ('A', 7500000, 8550000, 1.5),
    ('A', 8550000, 9650000, 1.75),
    ('A', 9650000, 10050000, 2),
    ('A', 10050000, 10350000, 2.25),
    ('A', 10350000, 10700000, 2.5),
    ('A', 10700000, 11050000, 3),
    ('A', 11050000, 11600000, 3.5),
    ('A', 11600000, 12500000, 4),
    ('A', 12500000, 13750000, 5),
    ('A', 13750000, 15100000, 6),
    ('A', 15100000, 16950000, 7),
    ('A', 16950000, 19750000, 8),
    ('A', 19750000, 24150000, 9),
    ('A', 24150000, 26450000, 10),
    ('A', 26450000, 28000000, 11),
    ('A', 28000000, 30050000, 12),
    ('A', 30050000, 32400000, 13),
    ('A', 32400000, 35400000, 14),
    ('A', 35400000, 39100000, 15),
    ('A', 39100000, 43850000, 16),
    ('A', 43850000, 47800000, 17),
    ('A', 47800000, 51400000, 18),
    ('A', 51400000, 56300000, 19),
    ('A', 56300000, 62200000, 20),
    ('A', 62200000, 68600000, 21),
    ('A', 68600000, 77500000, 22),
    ('A', 77500000, 89000000, 23),
    ('A', 89000000, 103000000, 24),
    ('A', 103000000, 125000000, 25),
    ('A', 125000000, 157000000, 26),
    ('A', 157000000, 206000000, 27),
    ('A', 206000000, 337000000, 28),
    ('A', 337000000, 454000000, 29),
    ('A', 454000000, 550000000, 30),
    ('A', 550000000, 695000000, 31),
    ('A', 695000000, 910000000, 32),
    ('A', 910000000, 1400000000, 33),
    ('A', 1400000000, NULL, 34),
    ('B', 0, 6200000, 0),
    ('B', 6200000, 6500000, 0.25),
    ('B', 6500000, 6850000, 0.5),
    ('B', 6850000, 7300000, 0.75),
    ('B', 7300000, 9200000, 1),
    ('B', 9200000, 10750000, 1.5),
    ('B', 10750000, 11250000, 2),
    ('B', 11250000, 11600000, 2.5),
    ('B', 11600000, 12600000, 3),
    ('B', 12600000, 13600000, 4),
    ('B', 13600000, 14950000, 5),
    ('B', 14950000, 16400000, 6),
    ('B', 16400000, 18450000, 7),
    ('B', 18450000, 21850000, 8),
    ('B', 21850000, 26000000, 9),
    ('B', 26000000, 27700000, 10),
    ('B', 27700000, 29350000, 11),
    ('B', 29350000, 31450000, 12),
    ('B', 31450000, 33950000, 13),
    ('B', 33950000, 37100000, 14),
    ('B', 37100000, 41100000, 15),
    ('B', 41100000, 45800000, 16),
    ('B', 45800000, 49500000, 17),
    ('B', 49500000, 53800000, 18),
    ('B', 53800000, 58500000, 19),
    ('B', 58500000, 64000000, 20),
    ('B', 64000000, 71000000, 21),
    ('B', 71000000, 80000000, 22),
    ('B', 80000000, 93000000, 23),
    ('B', 93000000, 109000000, 24),
    ('B', 109000000, 129000000, 25),
    ('B', 129000000, 163000000, 26),
    ('B', 163000000, 211000000, 27),
    ('B', 211000000, 374000000, 28),
    ('B', 374000000, 459000000, 29),
    ('B', 459000000, 555000000, 30),
    ('B', 555000000, 704000000, 31),
    ('B', 704000000, 957000000, 32),
    ('B', 957000000, 1405000000, 33),
    ('B', 1405000000, NULL, 34),
    ('C', 0, 6600000, 0),
    ('C', 6600000, 6950000, 0.25),
    ('C', 6950000, 7350000, 0.5),
    ('C', 7350000, 7800000, 0.75),
    ('C', 7800000, 8850000, 1),
    ('C', 8850000, 9800000, 1.25),
    ('C', 9800000, 10950000, 1.5),
    ('C', 10950000, 11200000, 1.75),
    ('C', 11200000, 12050000, 2),
    ('C', 12050000, 12950000, 3),
    ('C', 12950000, 14150000, 4),
    ('C', 14150000, 15550000, 5),
    ('C', 15550000, 17050000, 6),
    ('C', 17050000, 19500000, 7),
    ('C', 19500000, 22700000, 8),
    ('C', 22700000, 26600000, 9),
    ('C', 26600000, 28100000, 10),
    ('C', 28100000, 30100000, 11),
    ('C', 30100000, 32600000, 12),
    ('C', 32600000, 35400000, 13),
    ('C', 35400000, 38900000, 14),
    ('C', 38900000, 43000000, 15),
    ('C', 43000000, 47400000, 16),
    ('C', 47400000, 51200000, 17),
    ('C', 51200000, 55800000, 18),
    ('C', 55800000, 60400000, 19),
    ('C', 60400000, 66700000, 20),
    ('C', 66700000, 74500000, 21),
    ('C', 74500000, 83200000, 22),
    ('C', 83200000, 95600000, 23),
    ('C', 95600000, 110000000, 24),
    ('C', 110000000, 134000000, 25),
    ('C', 134000000, 169000000, 26),
    ('C', 169000000, 221000000, 27),
    ('C', 221000000, 390000000, 28),
    ('C', 390000000, 463000000, 29),
    ('C', 463000000, 561000000, 30),
    ('C', 561000000, 709000000, 31),
    ('C', 709000000, 965000000, 32),
    ('C', 965000000, 1419000000, 33),
    ('C', 1419000000, NULL, 34)
) AS v(category, lower_exclusive, upper_inclusive, rate_percent);

INSERT INTO core.tax_ptkp (status, annual_amount, effective_from) VALUES
    ('TK/0', 54000000, DATE '2016-01-01'), ('TK/1', 58500000, DATE '2016-01-01'),
    ('TK/2', 63000000, DATE '2016-01-01'), ('TK/3', 67500000, DATE '2016-01-01'),
    ('K/0', 58500000, DATE '2016-01-01'), ('K/1', 63000000, DATE '2016-01-01'),
    ('K/2', 67500000, DATE '2016-01-01'), ('K/3', 72000000, DATE '2016-01-01');

INSERT INTO core.tax_pasal17_bracket (upper_inclusive, rate_percent, effective_from) VALUES
    (60000000, 5, DATE '2022-01-01'), (250000000, 15, DATE '2022-01-01'),
    (500000000, 25, DATE '2022-01-01'), (5000000000, 30, DATE '2022-01-01'),
    (NULL, 35, DATE '2022-01-01');

INSERT INTO core.tax_setting (key, value, effective_from) VALUES
    ('biaya_jabatan_percent', 5, DATE '2009-01-01'),
    ('biaya_jabatan_max_annual', 6000000, DATE '2009-01-01'),
    ('partner_base_percent', 50, DATE '2024-01-01');

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000038', 'billing.fee.manage', 'Kelola aturan jasa tenaga kesehatan, pembayaran jasa, dan PPh 21', 'billing')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r JOIN core.permission p ON p.code = 'billing.fee.manage'
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'billing.fee.manage'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
