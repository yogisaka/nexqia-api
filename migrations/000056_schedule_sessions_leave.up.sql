-- nexqia-api/migrations/000056_schedule_sessions_leave.up.sql
-- #22 physician schedule (spec 2026-10-01-b-physician-schedule-design §3).

CREATE TABLE operations.shift (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    name         text NOT NULL,
    start_time   time NOT NULL,
    end_time     time NOT NULL,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_shift_time CHECK (end_time > start_time),
    CONSTRAINT chk_shift_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_shift_name ON operations.shift (merchant_id, name) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_shift_touch BEFORE UPDATE ON operations.shift FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.shift ENABLE ROW LEVEL SECURITY;
CREATE POLICY shift_isolation ON operations.shift
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_shift_tenant_check BEFORE INSERT OR UPDATE ON operations.shift
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

ALTER TABLE operations.physician_schedule
    ADD COLUMN room_id uuid REFERENCES core.location(id),
    ADD COLUMN quota_jkn integer NOT NULL DEFAULT 0 CHECK (quota_jkn >= 0),
    ADD COLUMN minutes_per_patient integer NOT NULL DEFAULT 10 CHECK (minutes_per_patient BETWEEN 1 AND 240),
    ADD COLUMN service_types text[] NOT NULL DEFAULT '{umum,bpjs}'
        CHECK (service_types <@ ARRAY['umum','bpjs','asuransi']::text[] AND cardinality(service_types) > 0),
    ADD COLUMN shift_id uuid REFERENCES operations.shift(id),
    ADD COLUMN notes text;
ALTER TABLE operations.physician_schedule
    ADD CONSTRAINT chk_physician_schedule_time CHECK (end_time > start_time),
    ADD CONSTRAINT chk_physician_schedule_slot_quota CHECK (slot_quota >= 0);

CREATE TABLE operations.physician_leave (
    id                      uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id              uuid NOT NULL REFERENCES core.company(id),
    merchant_id             uuid NOT NULL REFERENCES core.merchant(id),
    physician_id            uuid NOT NULL REFERENCES core.physician(id),
    date_from               date NOT NULL,
    date_to                 date NOT NULL,
    reason                  text,
    substitute_physician_id uuid REFERENCES core.physician(id),
    created_at              timestamptz NOT NULL DEFAULT now(),
    created_by              uuid,
    updated_at              timestamptz NOT NULL DEFAULT now(),
    updated_by              uuid,
    deleted_at              timestamptz,
    deleted_by              uuid,
    row_version             integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_physician_leave_range CHECK (date_to >= date_from),
    CONSTRAINT chk_physician_leave_substitute CHECK (substitute_physician_id IS NULL OR substitute_physician_id <> physician_id),
    CONSTRAINT chk_physician_leave_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE INDEX idx_physician_leave_lookup ON operations.physician_leave (merchant_id, physician_id, date_from) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_physician_leave_touch BEFORE UPDATE ON operations.physician_leave FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.physician_leave ENABLE ROW LEVEL SECURITY;
CREATE POLICY physician_leave_isolation ON operations.physician_leave
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_physician_leave_tenant_check BEFORE INSERT OR UPDATE ON operations.physician_leave
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE operations.schedule_session (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    schedule_id      uuid NOT NULL REFERENCES operations.physician_schedule(id),
    session_date     date NOT NULL,
    physician_id     uuid NOT NULL REFERENCES core.physician(id),
    department_id    uuid NOT NULL REFERENCES core.department(id),
    room_id          uuid REFERENCES core.location(id),
    start_time       time NOT NULL,
    end_time         time NOT NULL,
    quota_jkn        integer NOT NULL DEFAULT 0 CHECK (quota_jkn >= 0),
    slot_quota       integer NOT NULL DEFAULT 0 CHECK (slot_quota >= 0),
    status           text NOT NULL DEFAULT 'active' CHECK (status IN ('active','leave','substituted','cancelled')),
    leave_id         uuid REFERENCES operations.physician_leave(id),
    notes            text,
    bpjs_sync_status text NOT NULL DEFAULT 'not_synced' CHECK (bpjs_sync_status IN ('not_synced','synced','failed')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid,
    row_version      integer NOT NULL DEFAULT 1,
    UNIQUE (schedule_id, session_date),
    CONSTRAINT chk_schedule_session_time CHECK (end_time > start_time),
    CONSTRAINT chk_schedule_session_leave CHECK (status NOT IN ('leave','substituted') OR leave_id IS NOT NULL)
);
CREATE INDEX idx_schedule_session_date ON operations.schedule_session (merchant_id, session_date);
CREATE TRIGGER trg_schedule_session_touch BEFORE UPDATE ON operations.schedule_session FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.schedule_session ENABLE ROW LEVEL SECURITY;
CREATE POLICY schedule_session_isolation ON operations.schedule_session
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_schedule_session_tenant_check BEFORE INSERT OR UPDATE ON operations.schedule_session
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

ALTER TABLE core.physician ADD COLUMN sip_valid_until date;

ALTER TABLE operations.admission ADD COLUMN schedule_id uuid REFERENCES operations.physician_schedule(id);
CREATE INDEX idx_admission_schedule ON operations.admission (schedule_id, admission_at) WHERE schedule_id IS NOT NULL;
