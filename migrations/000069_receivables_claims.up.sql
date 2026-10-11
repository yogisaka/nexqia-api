-- nexqia-api/migrations/000069_receivables_claims.up.sql
-- Spec 2026-10-07-tariff-b2-receivables-claims §3.

ALTER TABLE core.payer
    ADD COLUMN payment_term_days integer NOT NULL DEFAULT 30 CHECK (payment_term_days > 0),
    ADD COLUMN claim_documents text[] NOT NULL DEFAULT '{}';

CREATE TABLE operations.claim_batch (
    id                  uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id          uuid NOT NULL REFERENCES core.company(id),
    merchant_id         uuid NOT NULL REFERENCES core.merchant(id),
    payer_id            uuid NOT NULL REFERENCES core.payer(id),
    kind                text NOT NULL CHECK (kind IN ('bpjs_noncapitation','bpjs_network','private')),
    period_start        date NOT NULL,
    period_end          date NOT NULL CHECK (period_end >= period_start),
    status              text NOT NULL DEFAULT 'draft' CHECK (status IN ('draft','submitted','verified','paid','closed')),
    submitted_at        timestamptz,
    submitted_by        uuid,
    submission_deadline date,
    documents           jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(documents) = 'object'),
    total_amount        numeric(15,2) NOT NULL DEFAULT 0,
    note                text,
    created_at          timestamptz NOT NULL DEFAULT now(),
    created_by          uuid,
    updated_at          timestamptz NOT NULL DEFAULT now(),
    updated_by          uuid,
    row_version         integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_claim_batch_submitted CHECK ((status = 'draft') = (submitted_at IS NULL))
);
CREATE INDEX idx_claim_batch_merchant ON operations.claim_batch (merchant_id, payer_id, period_end DESC);
CREATE TRIGGER trg_claim_batch_touch BEFORE UPDATE ON operations.claim_batch FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.claim_batch ENABLE ROW LEVEL SECURITY;
CREATE POLICY claim_batch_isolation ON operations.claim_batch USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_claim_batch_tenant_check BEFORE INSERT OR UPDATE ON operations.claim_batch FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.receivable (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    account_id        uuid NOT NULL REFERENCES operations.billing_account(id),
    admission_id      uuid NOT NULL REFERENCES operations.admission(id),
    payer_id          uuid NOT NULL REFERENCES core.payer(id),
    service_date_from date NOT NULL,
    service_date_to   date NOT NULL CHECK (service_date_to >= service_date_from),
    amount            numeric(15,2) NOT NULL CHECK (amount > 0),
    claim_types       text[] NOT NULL DEFAULT '{}',
    paid_amount       numeric(15,2) NOT NULL DEFAULT 0,
    adjusted_amount   numeric(15,2) NOT NULL DEFAULT 0 CHECK (adjusted_amount >= 0),
    status            text NOT NULL DEFAULT 'open' CHECK (status IN ('open','in_batch','submitted','partially_paid','paid','settled','disputed')),
    dispute_reason    text,
    due_date          date NOT NULL,
    expires_on        date,
    batch_id          uuid REFERENCES operations.claim_batch(id),
    documents         jsonb NOT NULL DEFAULT '{}'::jsonb CHECK (jsonb_typeof(documents) = 'object'),
    created_at        timestamptz NOT NULL DEFAULT now(),
    created_by        uuid,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    updated_by        uuid,
    row_version       integer NOT NULL DEFAULT 1,
    UNIQUE (account_id, payer_id),
    CONSTRAINT chk_receivable_balance CHECK (paid_amount >= 0 AND paid_amount + adjusted_amount <= amount),
    CONSTRAINT chk_receivable_dispute CHECK ((status = 'disputed') = (btrim(coalesce(dispute_reason, '')) <> ''))
);
CREATE INDEX idx_receivable_payer_status ON operations.receivable (merchant_id, payer_id, status);
CREATE INDEX idx_receivable_batch ON operations.receivable (batch_id);
CREATE TRIGGER trg_receivable_touch BEFORE UPDATE ON operations.receivable FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.receivable ENABLE ROW LEVEL SECURITY;
CREATE POLICY receivable_isolation ON operations.receivable USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_receivable_tenant_check BEFORE INSERT OR UPDATE ON operations.receivable FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.payer_payment (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    payer_id    uuid NOT NULL REFERENCES core.payer(id),
    batch_id    uuid REFERENCES operations.claim_batch(id),
    amount      numeric(15,2) NOT NULL CHECK (amount <> 0),
    paid_on     date NOT NULL,
    reference   text,
    note        text,
    reversal_of uuid UNIQUE REFERENCES operations.payer_payment(id),
    recorded_by uuid NOT NULL,
    recorded_at timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT chk_payer_payment_sign CHECK ((amount < 0) = (reversal_of IS NOT NULL))
);
ALTER TABLE operations.payer_payment ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_payment_isolation ON operations.payer_payment USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_payment_tenant_check BEFORE INSERT OR UPDATE ON operations.payer_payment FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON operations.payer_payment FROM app_runtime;

CREATE TABLE operations.payer_payment_allocation (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    payment_id    uuid NOT NULL REFERENCES operations.payer_payment(id),
    receivable_id uuid NOT NULL REFERENCES operations.receivable(id),
    amount        numeric(15,2) NOT NULL CHECK (amount <> 0),
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid NOT NULL
);
CREATE INDEX idx_payer_payment_allocation_receivable ON operations.payer_payment_allocation (receivable_id);
CREATE INDEX idx_payer_payment_allocation_payment ON operations.payer_payment_allocation (payment_id);
ALTER TABLE operations.payer_payment_allocation ENABLE ROW LEVEL SECURITY;
CREATE POLICY payer_payment_allocation_isolation ON operations.payer_payment_allocation USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_payer_payment_allocation_tenant_check BEFORE INSERT OR UPDATE ON operations.payer_payment_allocation FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON operations.payer_payment_allocation FROM app_runtime;

CREATE TABLE operations.receivable_adjustment (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    receivable_id uuid NOT NULL REFERENCES operations.receivable(id),
    kind          text NOT NULL CHECK (kind IN ('to_patient','write_off')),
    amount        numeric(15,2) NOT NULL CHECK (amount > 0),
    reason        text NOT NULL CHECK (btrim(reason) <> ''),
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid NOT NULL
);
CREATE INDEX idx_receivable_adjustment_receivable ON operations.receivable_adjustment (receivable_id);
ALTER TABLE operations.receivable_adjustment ENABLE ROW LEVEL SECURITY;
CREATE POLICY receivable_adjustment_isolation ON operations.receivable_adjustment USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_receivable_adjustment_tenant_check BEFORE INSERT OR UPDATE ON operations.receivable_adjustment FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
REVOKE UPDATE, DELETE ON operations.receivable_adjustment FROM app_runtime;

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000037', 'billing.receivable.manage', 'Kelola piutang penjamin, batch klaim, dan pembayaran penjamin', 'billing')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r JOIN core.permission p ON p.code = 'billing.receivable.manage'
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'billing.receivable.manage'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
