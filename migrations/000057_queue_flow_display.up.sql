-- nexqia-api/migrations/000057_queue_flow_display.up.sql
-- #23 configurable queue flows + display boards (spec 2026-10-01-c-queue-flow-display-design §3).

CREATE TABLE operations.queue_flow (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id     uuid NOT NULL REFERENCES core.company(id),
    merchant_id    uuid NOT NULL REFERENCES core.merchant(id),
    name           text NOT NULL,
    service_types  text[] NOT NULL DEFAULT '{umum,bpjs,asuransi}'
                   CHECK (service_types <@ ARRAY['umum','bpjs','asuransi']::text[] AND cardinality(service_types) > 0),
    department_ids uuid[] NOT NULL DEFAULT '{}',
    is_default     boolean NOT NULL DEFAULT false,
    is_active      boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    created_by     uuid,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     uuid,
    deleted_at     timestamptz,
    deleted_by     uuid,
    row_version    integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_queue_flow_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_queue_flow_default ON operations.queue_flow (merchant_id) WHERE is_default AND deleted_at IS NULL;
CREATE UNIQUE INDEX uq_queue_flow_name ON operations.queue_flow (merchant_id, name) WHERE deleted_at IS NULL;

CREATE TABLE operations.queue_stage (
    id                   uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id           uuid NOT NULL REFERENCES core.company(id),
    merchant_id          uuid NOT NULL REFERENCES core.merchant(id),
    flow_id              uuid NOT NULL REFERENCES operations.queue_flow(id) ON DELETE CASCADE,
    seq                  integer NOT NULL CHECK (seq >= 1),
    name                 text NOT NULL,
    kind                 text NOT NULL CHECK (kind IN ('admission','checkin','nurse','physician','cashier','pharmacy','support','custom')),
    number_prefix        text CHECK (number_prefix IS NULL OR number_prefix ~ '^[A-Z0-9]{1,4}$'),
    skippable            boolean NOT NULL DEFAULT false,
    requires_checkin     boolean NOT NULL DEFAULT false,
    bpjs_task_start      smallint CHECK (bpjs_task_start IS NULL OR bpjs_task_start BETWEEN 1 AND 7),
    bpjs_task_end        smallint CHECK (bpjs_task_end IS NULL OR bpjs_task_end BETWEEN 1 AND 7),
    served_by_permission text NOT NULL,
    created_at           timestamptz NOT NULL DEFAULT now(),
    updated_at           timestamptz NOT NULL DEFAULT now(),
    row_version          integer NOT NULL DEFAULT 1,
    UNIQUE (flow_id, seq) DEFERRABLE INITIALLY DEFERRED
);

ALTER TABLE operations.counter
    ADD COLUMN stage_id uuid REFERENCES operations.queue_stage(id),
    ADD COLUMN location_id uuid REFERENCES core.location(id),
    ADD COLUMN binding text NOT NULL DEFAULT 'fixed' CHECK (binding IN ('fixed','schedule_room'));

CREATE TABLE operations.queue_journey (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    admission_id     uuid NOT NULL REFERENCES operations.admission(id),
    person_id        uuid NOT NULL REFERENCES core.person(id),
    flow_id          uuid NOT NULL REFERENCES operations.queue_flow(id),
    current_stage_id uuid REFERENCES operations.queue_stage(id),
    status           text NOT NULL DEFAULT 'in_progress'
                     CHECK (status IN ('in_progress','awaiting_checkin','completed','cancelled')),
    created_at       timestamptz NOT NULL DEFAULT now(),
    created_by       uuid,
    updated_at       timestamptz NOT NULL DEFAULT now(),
    updated_by       uuid,
    row_version      integer NOT NULL DEFAULT 1,
    UNIQUE (admission_id)
);

ALTER TABLE operations.queue
    ADD COLUMN journey_id uuid REFERENCES operations.queue_journey(id),
    ADD COLUMN stage_id uuid REFERENCES operations.queue_stage(id),
    ADD COLUMN priority boolean NOT NULL DEFAULT false,
    ADD COLUMN checked_in_at timestamptz,
    ADD COLUMN started_at timestamptz,
    ADD COLUMN finished_at timestamptz,
    ADD COLUMN served_by uuid;
CREATE INDEX idx_queue_stage_waiting ON operations.queue (stage_id, status) WHERE status IN ('waiting','called');

CREATE TABLE operations.display_board (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    merchant_id       uuid NOT NULL REFERENCES core.merchant(id),
    name              text NOT NULL,
    location_id       uuid REFERENCES core.location(id),
    stage_ids         uuid[] NOT NULL DEFAULT '{}',
    counter_ids       uuid[] NOT NULL DEFAULT '{}',
    layout            text NOT NULL DEFAULT 'grid' CHECK (layout IN ('single','grid','split')),
    show_next_n       integer NOT NULL DEFAULT 5 CHECK (show_next_n BETWEEN 0 AND 20),
    voice             text NOT NULL DEFAULT 'chime' CHECK (voice IN ('off','chime','tts_id')),
    name_display      text NOT NULL DEFAULT 'hidden' CHECK (name_display IN ('hidden','initials','masked')),
    poll_seconds      integer NOT NULL DEFAULT 5 CHECK (poll_seconds BETWEEN 2 AND 60),
    token_hash        text,
    token_created_at  timestamptz,
    is_active         boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    created_by        uuid,
    updated_at        timestamptz NOT NULL DEFAULT now(),
    updated_by        uuid,
    deleted_at        timestamptz,
    deleted_by        uuid,
    row_version       integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_display_board_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE UNIQUE INDEX uq_display_board_token ON operations.display_board (token_hash) WHERE token_hash IS NOT NULL;

-- Tenant rules for every new table.
DO $$
DECLARE t text;
BEGIN
    FOREACH t IN ARRAY ARRAY['queue_flow','queue_stage','queue_journey','display_board'] LOOP
        EXECUTE format('ALTER TABLE operations.%I ENABLE ROW LEVEL SECURITY', t);
        EXECUTE format('CREATE POLICY %I ON operations.%I USING (merchant_id = current_setting(''app.current_merchant_id'')::uuid)', t || '_isolation', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE INSERT OR UPDATE ON operations.%I FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency()', 'trg_' || t || '_tenant_check', t);
        EXECUTE format('CREATE TRIGGER %I BEFORE UPDATE ON operations.%I FOR EACH ROW EXECUTE FUNCTION trg_touch_row()', 'trg_' || t || '_touch', t);
    END LOOP;
END;
$$;

-- Existing data: one default flow per merchant that already has counters or
-- queue rows, reproducing the old hardcoded pipeline pendaftaran -> perawat -> dokter
-- (number_prefix NULL keeps the old "<poli code>-NNN" numbering).
INSERT INTO operations.queue_flow (company_id, merchant_id, name, is_default)
SELECT DISTINCT m.company_id, m.id, 'Rawat Jalan (bawaan)', true
FROM core.merchant m
WHERE EXISTS (SELECT 1 FROM operations.counter c WHERE c.merchant_id = m.id)
   OR EXISTS (SELECT 1 FROM operations.queue q WHERE q.merchant_id = m.id);

INSERT INTO operations.queue_stage (company_id, merchant_id, flow_id, seq, name, kind, served_by_permission, bpjs_task_start, bpjs_task_end)
SELECT f.company_id, f.merchant_id, f.id, s.seq, s.name, s.kind, s.perm, s.ts, s.te
FROM operations.queue_flow f
CROSS JOIN (VALUES
    (1, 'Pendaftaran', 'admission', 'operations.counter.manage', 1::smallint, 3::smallint),
    (2, 'Perawat', 'nurse', 'operations.visit.manage', 3::smallint, 4::smallint),
    (3, 'Dokter', 'physician', 'operations.visit.manage', 4::smallint, 5::smallint)
) AS s(seq, name, kind, perm, ts, te)
WHERE f.name = 'Rawat Jalan (bawaan)';

UPDATE operations.counter c SET stage_id = st.id
FROM operations.queue_stage st
JOIN operations.queue_flow f ON f.id = st.flow_id AND f.name = 'Rawat Jalan (bawaan)'
WHERE st.merchant_id = c.merchant_id
  AND st.kind = CASE c.queue_type WHEN 'pendaftaran' THEN 'admission' WHEN 'perawat' THEN 'nurse' WHEN 'dokter' THEN 'physician' END;

UPDATE operations.queue q SET stage_id = st.id
FROM operations.queue_stage st
JOIN operations.queue_flow f ON f.id = st.flow_id AND f.name = 'Rawat Jalan (bawaan)'
WHERE st.merchant_id = q.merchant_id
  AND st.kind = CASE q.queue_type WHEN 'pendaftaran' THEN 'admission' WHEN 'perawat' THEN 'nurse' WHEN 'dokter' THEN 'physician' END;

-- Permission (spec §5), same pattern as 000053.
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000031', 'operations.queue.configure', 'Atur alur antrean, titik layanan, dan layar display', 'operations')
ON CONFLICT DO NOTHING;
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id
FROM core.role r JOIN core.permission p ON p.code = 'operations.queue.configure'
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;
INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'operations.queue.configure'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
