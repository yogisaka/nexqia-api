-- migrations/000033_core_diagnosis_config.up.sql
-- Configurable diagnosis coding standard: one company-level default row
-- (merchant_id IS NULL) plus an optional per-merchant override. `code_system`
-- must match a terminology.code_system.name (validated in the API layer,
-- not a DB FK — code_system is matched by name/uri elsewhere too, e.g.
-- terminology.search's system param).

CREATE TABLE core.diagnosis_config (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid REFERENCES core.merchant(id),
    code_system  text NOT NULL DEFAULT 'ICD-10',
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid
);

CREATE UNIQUE INDEX uq_diagnosis_config_company_default ON core.diagnosis_config (company_id) WHERE merchant_id IS NULL;
CREATE UNIQUE INDEX uq_diagnosis_config_merchant ON core.diagnosis_config (merchant_id) WHERE merchant_id IS NOT NULL;

CREATE TRIGGER trg_diagnosis_config_touch BEFORE UPDATE ON core.diagnosis_config
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

ALTER TABLE core.diagnosis_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY diagnosis_config_isolation ON core.diagnosis_config
    USING (company_id = current_setting('app.current_company_id')::uuid);
