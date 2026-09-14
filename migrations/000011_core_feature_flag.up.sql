-- migrations/000011_core_feature_flag.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §9 "Feature flag".

CREATE TABLE core.feature_flag (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    flag_key     text NOT NULL,      -- "clinical_form.consent_general", "ases.pasca_anestesi", dst
    flag_value   jsonb NOT NULL DEFAULT 'true',  -- boolean atau config kompleks (bukan cuma on/off)
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, flag_key)
);
CREATE TRIGGER trg_feature_flag_touch BEFORE UPDATE ON core.feature_flag
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.feature_flag ENABLE ROW LEVEL SECURITY;
CREATE POLICY feature_flag_isolation ON core.feature_flag
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
