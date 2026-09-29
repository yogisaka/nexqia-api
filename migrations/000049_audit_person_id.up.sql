-- nexqia-api/migrations/000049_audit_person_id.up.sql
-- #3b: the patient behind each log row, stored at write time (spec
-- 2026-09-29-audit-log-ui-design §3). No FK: logs must survive person deletion.

ALTER TABLE core.access_log ADD COLUMN person_id uuid;
ALTER TABLE core.audit_log  ADD COLUMN person_id uuid;

CREATE INDEX idx_access_log_person ON core.access_log (company_id, person_id, created_at);
CREATE INDEX idx_audit_log_person  ON core.audit_log  (company_id, person_id, changed_at);

-- Change log: 000046 recorder + person_id.
CREATE OR REPLACE FUNCTION core.trg_audit_row() RETURNS trigger
LANGUAGE plpgsql SECURITY DEFINER SET search_path = pg_catalog, core AS $$
DECLARE
    old_row jsonb := CASE WHEN TG_OP <> 'INSERT' THEN to_jsonb(OLD) END;
    -- <> 'DELETE', not 000046's <> 'INSERT': NEW exists on INSERT and UPDATE; only
    -- DELETE lacks it. The 000046 copy left new_row NULL on INSERT, so the recorder
    -- wrote record_id NULL (SQLSTATE 23502) whenever the session GUC supplied the
    -- company (e.g. self-registration).
    new_row jsonb := CASE WHEN TG_OP <> 'DELETE' THEN to_jsonb(NEW) END;
    cur_row jsonb := COALESCE(new_row, old_row);
    v_company uuid := COALESCE(
        (cur_row->>'company_id')::uuid,
        NULLIF(current_setting('app.current_company_id', true), '')::uuid);
    v_fields text[] := '{}';
    v_person uuid;
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
    v_person := CASE TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME
        WHEN 'core.person' THEN (cur_row->>'id')::uuid
        WHEN 'operations.admission_guarantor' THEN
            (SELECT a.person_id FROM operations.admission a WHERE a.id = (cur_row->>'admission_id')::uuid)
        ELSE (cur_row->>'person_id')::uuid
    END;
    INSERT INTO core.audit_log
        (company_id, merchant_id, table_name, record_id, action, changed_fields, changed_by, platform_admin_id, person_id)
    VALUES (
        v_company,
        (cur_row->>'merchant_id')::uuid,
        TG_TABLE_SCHEMA || '.' || TG_TABLE_NAME,
        (cur_row->>'id')::uuid,
        lower(TG_OP),
        v_fields,
        NULLIF(current_setting('app.current_user_id', true), '')::uuid,
        NULLIF(current_setting('app.platform_admin_id', true), '')::uuid,
        v_person);
    RETURN NULL;
END;
$$;

-- Access log: fill person_id from the resource when the app did not pass one.
-- Runs as the caller (not DEFINER): a visit/queue the session cannot see stays NULL.
CREATE FUNCTION core.trg_access_log_person() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    IF NEW.person_id IS NULL AND NEW.resource_id IS NOT NULL THEN
        NEW.person_id := CASE NEW.resource
            WHEN 'person' THEN NEW.resource_id
            WHEN 'person_family' THEN NEW.resource_id
            WHEN 'patient_allergy' THEN NEW.resource_id
            WHEN 'admission' THEN (SELECT a.person_id FROM operations.admission a WHERE a.id = NEW.resource_id)
            WHEN 'queue' THEN (SELECT q.person_id FROM operations.queue q WHERE q.id = NEW.resource_id)
        END;
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_access_log_person BEFORE INSERT ON core.access_log
    FOR EACH ROW EXECUTE FUNCTION core.trg_access_log_person();

-- Backfill (migration role owns the tables: no RLS). Rows whose source row is gone stay NULL.
UPDATE core.audit_log SET person_id = record_id
 WHERE table_name = 'core.person' AND person_id IS NULL;
UPDATE core.audit_log l SET person_id = t.person_id FROM core.person_family t
 WHERE l.table_name = 'core.person_family' AND t.id = l.record_id AND l.person_id IS NULL;
UPDATE core.audit_log l SET person_id = t.person_id FROM core.patient_allergy t
 WHERE l.table_name = 'core.patient_allergy' AND t.id = l.record_id AND l.person_id IS NULL;
UPDATE core.audit_log l SET person_id = t.person_id FROM operations.admission t
 WHERE l.table_name = 'operations.admission' AND t.id = l.record_id AND l.person_id IS NULL;
UPDATE core.audit_log l SET person_id = t.person_id FROM operations.queue t
 WHERE l.table_name = 'operations.queue' AND t.id = l.record_id AND l.person_id IS NULL;
UPDATE core.audit_log l SET person_id = a.person_id
  FROM operations.admission_guarantor g JOIN operations.admission a ON a.id = g.admission_id
 WHERE l.table_name = 'operations.admission_guarantor' AND g.id = l.record_id AND l.person_id IS NULL;

UPDATE core.access_log SET person_id = resource_id
 WHERE resource IN ('person', 'person_family', 'patient_allergy') AND resource_id IS NOT NULL AND person_id IS NULL;
UPDATE core.access_log l SET person_id = t.person_id FROM operations.admission t
 WHERE l.resource = 'admission' AND t.id = l.resource_id AND l.person_id IS NULL;
UPDATE core.access_log l SET person_id = t.person_id FROM operations.queue t
 WHERE l.resource = 'queue' AND t.id = l.resource_id AND l.person_id IS NULL;
