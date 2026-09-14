-- migrations/000007_core_physician.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §5 "Physician".

CREATE TABLE core.physician (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    person_id     uuid NOT NULL REFERENCES core.person(id),
    str_number    text,                       -- Surat Tanda Registrasi
    sip_number    text,                       -- Surat Izin Praktik
    specialty_concept_id uuid REFERENCES terminology.concept(id),
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    deleted_at    timestamptz,
    deleted_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, person_id),
    CONSTRAINT chk_physician_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_physician_merchant ON core.physician (merchant_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_physician_touch BEFORE UPDATE ON core.physician
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.physician ENABLE ROW LEVEL SECURITY;
CREATE POLICY physician_isolation ON core.physician
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_physician_tenant_check BEFORE INSERT OR UPDATE ON core.physician
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
