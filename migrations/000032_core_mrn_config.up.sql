-- migrations/000032_core_mrn_config.up.sql
-- Configurable medical_record_no format: one company-level default row
-- (merchant_id IS NULL) plus an optional per-merchant override row.
-- format uses template tokens: {YYYY} {YY} {MM} {MERCHANT_CODE} {SEQ:N}
-- (N = zero-padded sequence width), e.g. "RM-{YYYY}-{SEQ:5}".

CREATE TABLE core.mrn_config (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid REFERENCES core.merchant(id),
    format       text NOT NULL DEFAULT 'RM-{YYYY}-{SEQ:5}',
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid
);

-- At most one company-level default (merchant_id IS NULL) and at most one
-- override per merchant.
CREATE UNIQUE INDEX uq_mrn_config_company_default ON core.mrn_config (company_id) WHERE merchant_id IS NULL;
CREATE UNIQUE INDEX uq_mrn_config_merchant ON core.mrn_config (merchant_id) WHERE merchant_id IS NOT NULL;

CREATE TRIGGER trg_mrn_config_touch BEFORE UPDATE ON core.mrn_config
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

ALTER TABLE core.mrn_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY mrn_config_isolation ON core.mrn_config
    USING (company_id = current_setting('app.current_company_id')::uuid);
