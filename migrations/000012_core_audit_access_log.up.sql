-- migrations/000012_core_audit_access_log.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §10 "Audit log & Access log".

-- data-change audit — siapa UBAH apa
CREATE TABLE core.audit_log (
    id             uuid NOT NULL DEFAULT uuid_generate_v7(),
    company_id     uuid NOT NULL,
    merchant_id    uuid,
    table_name     text NOT NULL,
    record_id      uuid NOT NULL,
    action         text NOT NULL CHECK (action IN ('insert','update','delete')),
    old_value      jsonb,
    new_value      jsonb,
    changed_by     uuid NOT NULL,
    changed_at     timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, changed_at)
) PARTITION BY RANGE (changed_at);
CREATE INDEX idx_audit_log_record ON core.audit_log (table_name, record_id, changed_at);
ALTER TABLE core.audit_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY audit_log_isolation ON core.audit_log
    USING (company_id = current_setting('app.current_company_id')::uuid);
CREATE TRIGGER trg_audit_log_tenant_check BEFORE INSERT OR UPDATE ON core.audit_log
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
SELECT partman.create_parent(
    p_parent_table => 'core.audit_log',
    p_control      => 'changed_at',
    p_interval     => '1 month'
);

-- access log — siapa LIHAT apa (wajib Permenkes 24/2022 Ps.28, terpisah dari audit perubahan data)
CREATE TABLE core.access_log (
    id           uuid NOT NULL DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL,
    merchant_id  uuid,
    actor_id     uuid NOT NULL,
    resource     text NOT NULL,      -- "patient_record", "clinical_form", dst
    resource_id  uuid,
    action       text NOT NULL,      -- "view","export","print"
    ip_address   inet,
    created_at   timestamptz NOT NULL DEFAULT now(),
    PRIMARY KEY (id, created_at)
) PARTITION BY RANGE (created_at);
CREATE INDEX idx_access_log_actor ON core.access_log (actor_id, created_at);
ALTER TABLE core.access_log ENABLE ROW LEVEL SECURITY;
CREATE POLICY access_log_isolation ON core.access_log
    USING (company_id = current_setting('app.current_company_id')::uuid);
CREATE TRIGGER trg_access_log_tenant_check BEFORE INSERT OR UPDATE ON core.access_log
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
SELECT partman.create_parent(
    p_parent_table => 'core.access_log',
    p_control      => 'created_at',
    p_interval     => '1 month'
);
