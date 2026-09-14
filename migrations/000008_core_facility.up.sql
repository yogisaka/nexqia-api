-- migrations/000008_core_facility.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §6 "Master Data fasilitas".

CREATE TABLE core.department (   -- "department" = poli/spesialisasi
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    code         text NOT NULL,
    name         text NOT NULL,
    specialty_concept_id uuid REFERENCES terminology.concept(id),
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_department_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_department_merchant ON core.department (merchant_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_department_touch BEFORE UPDATE ON core.department
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.department ENABLE ROW LEVEL SECURITY;
CREATE POLICY department_isolation ON core.department
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_department_tenant_check BEFORE INSERT OR UPDATE ON core.department
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.ward (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    department_id uuid REFERENCES core.department(id),
    code         text NOT NULL,
    name         text NOT NULL,
    ward_class   text,                        -- kelas: VIP/I/II/III sesuai standar BPJS
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (merchant_id, code),
    CONSTRAINT chk_ward_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_ward_merchant ON core.ward (merchant_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_ward_touch BEFORE UPDATE ON core.ward
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.ward ENABLE ROW LEVEL SECURITY;
CREATE POLICY ward_isolation ON core.ward
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_ward_tenant_check BEFORE INSERT OR UPDATE ON core.ward
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

CREATE TABLE core.bed (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id   uuid NOT NULL REFERENCES core.company(id),
    merchant_id  uuid NOT NULL REFERENCES core.merchant(id),
    ward_id      uuid NOT NULL REFERENCES core.ward(id),
    code         text NOT NULL,
    status       text NOT NULL DEFAULT 'available'
                 CHECK (status IN ('available','occupied','maintenance','reserved')),
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid,
    updated_at   timestamptz NOT NULL DEFAULT now(),
    updated_by   uuid,
    deleted_at   timestamptz,
    deleted_by   uuid,
    row_version  integer NOT NULL DEFAULT 1,
    UNIQUE (ward_id, code),
    CONSTRAINT chk_bed_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_bed_ward ON core.bed (ward_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_bed_status ON core.bed (merchant_id, status) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_bed_touch BEFORE UPDATE ON core.bed
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.bed ENABLE ROW LEVEL SECURITY;
CREATE POLICY bed_isolation ON core.bed
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_bed_tenant_check BEFORE INSERT OR UPDATE ON core.bed
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
