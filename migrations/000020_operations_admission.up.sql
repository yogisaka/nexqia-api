-- migrations/000020_operations_admission.up.sql
-- Verbatim transcript of docs/08-operations-ddl.md §2 "Admission" — core columns
-- only, satellite tables (admission_guarantor/admission_bed_history/admission_dpjp/
-- medical_record_tracking) deferred, see
-- 2026-09-16-v1-operations-antrian-jadwal-design.md §2.

CREATE TABLE operations.admission (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),
    merchant_id     uuid NOT NULL REFERENCES core.merchant(id),
    visit_no        text NOT NULL,
    person_id       uuid NOT NULL REFERENCES core.person(id),
    admission_type  text NOT NULL CHECK (admission_type IN ('outpatient','inpatient','emergency')),
    department_id   uuid NOT NULL REFERENCES core.department(id),
    physician_id    uuid REFERENCES core.physician(id),
    ward_id         uuid REFERENCES core.ward(id),
    bed_id          uuid REFERENCES core.bed(id),
    primary_payer_id uuid REFERENCES core.payer(id),
    bpjs_sep_number text,
    status          text NOT NULL DEFAULT 'registered'
                    CHECK (status IN ('registered','admitted','in_treatment','discharged','cancelled')),
    admission_at    timestamptz NOT NULL DEFAULT now(),
    discharge_at    timestamptz,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      uuid,
    deleted_at      timestamptz,
    deleted_by      uuid,
    row_version     integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, visit_no),
    CONSTRAINT chk_admission_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_admission_person ON operations.admission (person_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_admission_merchant_status ON operations.admission (merchant_id, status) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_admission_touch BEFORE UPDATE ON operations.admission
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE operations.admission ENABLE ROW LEVEL SECURITY;
CREATE POLICY admission_isolation ON operations.admission
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_admission_tenant_check BEFORE INSERT OR UPDATE ON operations.admission
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

ALTER TABLE operations.queue
    ADD CONSTRAINT fk_queue_admission FOREIGN KEY (admission_id) REFERENCES operations.admission(id);
