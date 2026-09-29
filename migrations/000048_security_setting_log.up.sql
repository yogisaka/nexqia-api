-- nexqia-api/migrations/000048_security_setting_log.up.sql
-- #10b: who changed which merchant security setting, from what to what (spec
-- 2026-09-29-merchant-security-settings-design §6). Values are stored: security
-- settings are not patient data. Written only by the trigger below.
CREATE TABLE core.security_setting_log (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    flag_key          text NOT NULL,
    old_value         jsonb,
    new_value         jsonb,
    changed_by        uuid,
    platform_admin_id uuid,
    changed_at        timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_security_setting_log_merchant_time ON core.security_setting_log (merchant_id, changed_at);
ALTER TABLE core.security_setting_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY security_setting_log_isolation ON core.security_setting_log
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
-- Default privileges on schema core grant all four to app_runtime (000002): read-only here.
REVOKE INSERT, UPDATE, DELETE ON core.security_setting_log FROM app_runtime;

CREATE FUNCTION core.trg_security_setting_log() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, core AS $$
DECLARE
    v_key      text  := COALESCE(NEW.flag_key, OLD.flag_key);
    v_merchant uuid  := COALESCE(NEW.merchant_id, OLD.merchant_id);
    v_old      jsonb := CASE WHEN TG_OP <> 'INSERT' THEN OLD.flag_value END;
    v_new      jsonb := CASE WHEN TG_OP <> 'DELETE' THEN NEW.flag_value END;
BEGIN
    IF v_key NOT LIKE 'auth.%' THEN
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' AND v_old IS NOT DISTINCT FROM v_new AND OLD.flag_key = NEW.flag_key THEN
        RETURN NULL; -- saved without a change
    END IF;
    INSERT INTO core.security_setting_log
        (company_id, merchant_id, flag_key, old_value, new_value, changed_by, platform_admin_id)
    SELECT m.company_id, v_merchant, v_key, v_old, v_new,
           NULLIF(current_setting('app.current_user_id', true), '')::uuid,
           NULLIF(current_setting('app.platform_admin_id', true), '')::uuid
    FROM core.merchant m WHERE m.id = v_merchant;
    RETURN NULL;
END;
$$;
CREATE TRIGGER trg_security_setting_log AFTER INSERT OR UPDATE OR DELETE ON core.feature_flag
    FOR EACH ROW EXECUTE FUNCTION core.trg_security_setting_log();