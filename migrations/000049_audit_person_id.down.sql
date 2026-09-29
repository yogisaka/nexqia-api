-- nexqia-api/migrations/000049_audit_person_id.down.sql
-- Restore the 000046 recorder (no person_id), then drop the #3b additions.
DROP TRIGGER IF EXISTS trg_access_log_person ON core.access_log;
DROP FUNCTION IF EXISTS core.trg_access_log_person();

-- 000046 version of the recorder, verbatim.
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

DROP INDEX IF EXISTS core.idx_audit_log_person;
DROP INDEX IF EXISTS core.idx_access_log_person;
ALTER TABLE core.audit_log DROP COLUMN IF EXISTS person_id;
ALTER TABLE core.access_log DROP COLUMN IF EXISTS person_id;
