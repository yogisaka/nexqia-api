-- nexqia-api/migrations/000046_audit_trail.up.sql
-- K1 audit trail, see 2026-09-29-audit-trail-design.md.
-- audit_log: WHO changed WHICH fields of WHICH patient-data row — never the values
-- (no second copy of patient data). access_log: WHO read WHICH patient resource.

ALTER TABLE core.audit_log DROP COLUMN old_value;
ALTER TABLE core.audit_log DROP COLUMN new_value;
ALTER TABLE core.audit_log ALTER COLUMN changed_by DROP NOT NULL; -- NULL = no user session (self-registration, platform admin action)
ALTER TABLE core.audit_log ADD COLUMN changed_fields text[] NOT NULL DEFAULT '{}';
ALTER TABLE core.audit_log ADD COLUMN platform_admin_id uuid;        -- platform.admin_user.id behind the change (impersonation session or direct platform endpoint)

ALTER TABLE core.access_log ADD COLUMN platform_admin_id uuid;
ALTER TABLE core.access_log ADD COLUMN status_code smallint NOT NULL DEFAULT 0;

CREATE INDEX idx_audit_log_company_time ON core.audit_log (company_id, changed_at);
CREATE INDEX idx_access_log_resource ON core.access_log (resource, resource_id, created_at);

-- Generic row-change recorder. SECURITY DEFINER (owner = migration role, which owns
-- core.audit_log and is not subject to its RLS) so the insert works even where the
-- request has no app.current_company_id (self-registration, platform admin paths).
-- The company comes from the audited row itself, falling back to the session GUC for
-- child tables without company_id (operations.admission_guarantor).
CREATE OR REPLACE FUNCTION core.trg_audit_row() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, core AS $$
DECLARE
    old_row jsonb := CASE WHEN TG_OP <> 'INSERT' THEN to_jsonb(OLD) END;
    new_row jsonb := CASE WHEN TG_OP <> 'DELETE' THEN to_jsonb(NEW) END;
    cur_row jsonb := COALESCE(new_row, old_row);
    v_company uuid := COALESCE(
        (cur_row->>'company_id')::uuid,
        NULLIF(current_setting('app.current_company_id', true), '')::uuid);
    v_fields text[] := '{}';
BEGIN
    IF v_company IS NULL THEN
        RAISE WARNING 'audit: no company for %.% id %, change not recorded',
            TG_TABLE_SCHEMA, TG_TABLE_NAME, cur_row->>'id';
        RETURN NULL;
    END IF;
    IF TG_OP = 'UPDATE' THEN
        SELECT COALESCE(array_agg(k ORDER BY k), '{}') INTO v_fields
        FROM jsonb_object_keys(new_row) AS k
        WHERE k NOT IN ('updated_at', 'updated_by', 'row_version')
          AND new_row->k IS DISTINCT FROM old_row->k;
        IF cardinality(v_fields) = 0 THEN
            RETURN NULL; -- touch-only update, nothing changed
        END IF;
    END IF;
    INSERT INTO core.audit_log
        (company_id, merchant_id, table_name, record_id, action, changed_fields, changed_by, platform_admin_id)
    VALUES (
        v_company,
        (cur_row->>'merchant_id')::uuid,
        TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME,
        (cur_row->>'id')::uuid,
        lower(TG_OP),
        v_fields,
        NULLIF(current_setting('app.current_user_id', true), '')::uuid,
        NULLIF(current_setting('app.platform_admin_id', true), '')::uuid);
    RETURN NULL;
END;
$$;

CREATE TRIGGER trg_audit_person AFTER INSERT OR UPDATE OR DELETE ON core.person
    FOR EACH ROW EXECUTE FUNCTION core.trg_audit_row();
CREATE TRIGGER trg_audit_person_family AFTER INSERT OR UPDATE OR DELETE ON core.person_family
    FOR EACH ROW EXECUTE FUNCTION core.trg_audit_row();
CREATE TRIGGER trg_audit_patient_allergy AFTER INSERT OR UPDATE OR DELETE ON core.patient_allergy
    FOR EACH ROW EXECUTE FUNCTION core.trg_audit_row();
CREATE TRIGGER trg_audit_admission AFTER INSERT OR UPDATE OR DELETE ON operations.admission
    FOR EACH ROW EXECUTE FUNCTION core.trg_audit_row();
CREATE TRIGGER trg_audit_admission_guarantor AFTER INSERT OR UPDATE OR DELETE ON operations.admission_guarantor
    FOR EACH ROW EXECUTE FUNCTION core.trg_audit_row();
CREATE TRIGGER trg_audit_queue AFTER INSERT OR UPDATE OR DELETE ON operations.queue
    FOR EACH ROW EXECUTE FUNCTION core.trg_audit_row();
