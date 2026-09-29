-- nexqia-api/migrations/000047_child_table_tenant_rls.up.sql
-- T6 (docs/20-security-compliance-gap-audit.md): tenant columns + RLS on four child
-- tables that relied on app-side joins for isolation. See
-- docs/design/specs/2026-09-29-child-rls-token-storage-design.md §1.
-- The columns are ALWAYS filled from the parent row by a BEFORE trigger (runs as the
-- caller, so the parent read goes through the parent's RLS): attaching a child to a
-- parent the session cannot see yields NULL and NOT NULL rejects the write.

-- 1. Columns
ALTER TABLE operations.queue_status_history
    ADD COLUMN company_id  uuid REFERENCES core.company(id),
    ADD COLUMN merchant_id uuid REFERENCES core.merchant(id);
ALTER TABLE operations.admission_guarantor
    ADD COLUMN company_id  uuid REFERENCES core.company(id),
    ADD COLUMN merchant_id uuid REFERENCES core.merchant(id);
ALTER TABLE core.media_link
    ADD COLUMN company_id  uuid REFERENCES core.company(id),
    ADD COLUMN merchant_id uuid REFERENCES core.merchant(id);
ALTER TABLE core.person_merge_log
    ADD COLUMN company_id  uuid REFERENCES core.company(id);

-- 2. Backfill existing rows from the parent (migration role owns the tables: no RLS here)
UPDATE operations.queue_status_history h
   SET company_id = q.company_id, merchant_id = q.merchant_id
  FROM operations.queue q WHERE q.id = h.queue_id;
UPDATE operations.admission_guarantor g
   SET company_id = a.company_id, merchant_id = a.merchant_id
  FROM operations.admission a WHERE a.id = g.admission_id;
UPDATE core.media_link l
   SET company_id = m.company_id, merchant_id = m.merchant_id
  FROM core.media m WHERE m.id = l.media_id;
UPDATE core.person_merge_log l
   SET company_id = p.company_id
  FROM core.person p WHERE p.id = l.surviving_person_id;

ALTER TABLE operations.queue_status_history
    ALTER COLUMN company_id SET NOT NULL, ALTER COLUMN merchant_id SET NOT NULL;
ALTER TABLE operations.admission_guarantor
    ALTER COLUMN company_id SET NOT NULL, ALTER COLUMN merchant_id SET NOT NULL;
ALTER TABLE core.media_link
    ALTER COLUMN company_id SET NOT NULL, ALTER COLUMN merchant_id SET NOT NULL;
ALTER TABLE core.person_merge_log
    ALTER COLUMN company_id SET NOT NULL;

-- 3. Fill-from-parent triggers. Named trg_00_* so they run before any other BEFORE
--    trigger on the same table (Postgres fires same-timing triggers in name order).
CREATE FUNCTION operations.trg_fill_tenant_queue_status_history() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.company_id  := (SELECT q.company_id  FROM operations.queue q WHERE q.id = NEW.queue_id);
    NEW.merchant_id := (SELECT q.merchant_id FROM operations.queue q WHERE q.id = NEW.queue_id);
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_00_fill_tenant BEFORE INSERT OR UPDATE ON operations.queue_status_history
    FOR EACH ROW EXECUTE FUNCTION operations.trg_fill_tenant_queue_status_history();

CREATE FUNCTION operations.trg_fill_tenant_admission_guarantor() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.company_id  := (SELECT a.company_id  FROM operations.admission a WHERE a.id = NEW.admission_id);
    NEW.merchant_id := (SELECT a.merchant_id FROM operations.admission a WHERE a.id = NEW.admission_id);
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_00_fill_tenant BEFORE INSERT OR UPDATE ON operations.admission_guarantor
    FOR EACH ROW EXECUTE FUNCTION operations.trg_fill_tenant_admission_guarantor();

CREATE FUNCTION core.trg_fill_tenant_media_link() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.company_id  := (SELECT m.company_id  FROM core.media m WHERE m.id = NEW.media_id);
    NEW.merchant_id := (SELECT m.merchant_id FROM core.media m WHERE m.id = NEW.media_id);
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_00_fill_tenant BEFORE INSERT OR UPDATE ON core.media_link
    FOR EACH ROW EXECUTE FUNCTION core.trg_fill_tenant_media_link();

CREATE FUNCTION core.trg_fill_tenant_person_merge_log() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.company_id := (SELECT p.company_id FROM core.person p WHERE p.id = NEW.surviving_person_id);
    IF NEW.company_id IS NOT NULL AND NOT EXISTS (
        SELECT 1 FROM core.person p
         WHERE p.id = NEW.merged_person_id AND p.company_id = NEW.company_id
    ) THEN
        RAISE EXCEPTION 'person merge across companies is not allowed';
    END IF;
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_00_fill_tenant BEFORE INSERT OR UPDATE ON core.person_merge_log
    FOR EACH ROW EXECUTE FUNCTION core.trg_fill_tenant_person_merge_log();

-- 4. RLS (same shape as the parent tables' policies)
ALTER TABLE operations.queue_status_history ENABLE ROW LEVEL SECURITY;
CREATE POLICY queue_status_history_isolation ON operations.queue_status_history
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
ALTER TABLE operations.admission_guarantor ENABLE ROW LEVEL SECURITY;
CREATE POLICY admission_guarantor_isolation ON operations.admission_guarantor
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
ALTER TABLE core.media_link ENABLE ROW LEVEL SECURITY;
CREATE POLICY media_link_isolation ON core.media_link
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
ALTER TABLE core.person_merge_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY person_merge_log_isolation ON core.person_merge_log
    USING (company_id = current_setting('app.current_company_id')::uuid);