BEGIN;

CREATE TABLE operations.visit_token (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    admission_id uuid NOT NULL REFERENCES operations.admission(id),
    token_hash   text NOT NULL,
    media        text NOT NULL DEFAULT 'slip' CHECK (media IN ('none','slip','wristband','both')),
    expires_at   timestamptz NOT NULL,
    closed_at    timestamptz,
    closed_by    uuid REFERENCES core.app_user(id),
    close_reason text CHECK (close_reason IN ('journey_completed','journey_cancelled','reissued','revoked')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid NOT NULL REFERENCES core.app_user(id),
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid REFERENCES core.app_user(id),
    row_version  integer NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX uq_visit_token_hash ON operations.visit_token (token_hash);
CREATE UNIQUE INDEX uq_visit_token_active ON operations.visit_token (admission_id) WHERE closed_at IS NULL;
CREATE TRIGGER trg_visit_token_touch BEFORE UPDATE ON operations.visit_token
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE TRIGGER trg_visit_token_tenant_check BEFORE INSERT OR UPDATE ON operations.visit_token
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE operations.visit_token ENABLE ROW LEVEL SECURITY;
CREATE POLICY visit_token_isolation ON operations.visit_token
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

CREATE TABLE operations.visit_media_config (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    department_id uuid REFERENCES core.department(id),
    media         text NOT NULL DEFAULT 'slip' CHECK (media IN ('none','slip','wristband','both')),
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid NOT NULL REFERENCES core.app_user(id),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid REFERENCES core.app_user(id),
    row_version   integer NOT NULL DEFAULT 1
);
CREATE UNIQUE INDEX uq_visit_media_config ON operations.visit_media_config (merchant_id, COALESCE(department_id, '00000000-0000-0000-0000-000000000000'::uuid));
CREATE TRIGGER trg_visit_media_config_touch BEFORE UPDATE ON operations.visit_media_config
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE TRIGGER trg_visit_media_config_tenant_check BEFORE INSERT OR UPDATE ON operations.visit_media_config
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE operations.visit_media_config ENABLE ROW LEVEL SECURITY;
CREATE POLICY visit_media_config_isolation ON operations.visit_media_config
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

ALTER TABLE operations.admission ADD COLUMN fall_risk boolean NOT NULL DEFAULT false;

ALTER TABLE core.person ADD COLUMN card_token_hash text;
ALTER TABLE core.person ADD COLUMN card_issued_at timestamptz;
ALTER TABLE core.person ADD COLUMN card_issued_by uuid REFERENCES core.app_user(id);
CREATE UNIQUE INDEX uq_person_card_token ON core.person (card_token_hash) WHERE card_token_hash IS NOT NULL;

COMMIT;
