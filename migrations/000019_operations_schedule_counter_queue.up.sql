-- migrations/000019_operations_schedule_counter_queue.up.sql
-- Verbatim transcript of docs/08-operations-ddl.md §1 "Penjadwalan & Antrian",
-- minus operations.appointment (deferred to v1.1, see
-- 2026-09-16-v1-operations-antrian-jadwal-design.md §2).

CREATE TABLE operations.physician_schedule (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    physician_id  uuid NOT NULL REFERENCES core.physician(id),
    department_id uuid NOT NULL REFERENCES core.department(id),
    day_of_week   smallint NOT NULL CHECK (day_of_week BETWEEN 0 AND 6),
    start_time    time NOT NULL,
    end_time      time NOT NULL,
    slot_quota    integer NOT NULL DEFAULT 0,
    effective_from date NOT NULL,
    effective_to   date,
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    deleted_at    timestamptz,
    deleted_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_physician_schedule_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_physician_schedule_lookup ON operations.physician_schedule
    (merchant_id, physician_id, day_of_week) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_physician_schedule_touch BEFORE UPDATE ON operations.physician_schedule
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.physician_schedule ENABLE ROW LEVEL SECURITY;
CREATE POLICY physician_schedule_isolation ON operations.physician_schedule
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_physician_schedule_tenant_check BEFORE INSERT OR UPDATE ON operations.physician_schedule
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.counter (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    queue_type    text NOT NULL,
    department_id uuid REFERENCES core.department(id),
    code          text NOT NULL,
    name          text,
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, queue_type, code)
);
CREATE TRIGGER trg_counter_touch BEFORE UPDATE ON operations.counter
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.counter ENABLE ROW LEVEL SECURITY;
CREATE POLICY counter_isolation ON operations.counter
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_counter_tenant_check BEFORE INSERT OR UPDATE ON operations.counter
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.queue (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    queue_type    text NOT NULL,
    department_id uuid REFERENCES core.department(id),
    person_id     uuid NOT NULL REFERENCES core.person(id),
    admission_id  uuid,
    counter_id    uuid REFERENCES operations.counter(id),
    queue_number  text NOT NULL,
    status        text NOT NULL DEFAULT 'waiting'
                  CHECK (status IN ('waiting','called','in_progress','done','skipped','cancelled')),
    called_at     timestamptz,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    row_version   integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_queue_active ON operations.queue (merchant_id, queue_type, status)
    WHERE status IN ('waiting','called');
CREATE INDEX idx_queue_admission ON operations.queue (admission_id);
CREATE TRIGGER trg_queue_touch BEFORE UPDATE ON operations.queue
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.queue ENABLE ROW LEVEL SECURITY;
CREATE POLICY queue_isolation ON operations.queue
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_queue_tenant_check BEFORE INSERT OR UPDATE ON operations.queue
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.queue_status_history (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    queue_id      uuid NOT NULL REFERENCES operations.queue(id),
    counter_id    uuid REFERENCES operations.counter(id),
    from_status   text,
    to_status     text NOT NULL,
    changed_by    uuid NOT NULL,
    changed_at    timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_queue_status_history_queue ON operations.queue_status_history (queue_id);
