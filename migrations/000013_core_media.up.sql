-- migrations/000013_core_media.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §11 "Media Management".

CREATE TABLE core.media (
    id               uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id       uuid NOT NULL REFERENCES core.company(id),
    merchant_id      uuid NOT NULL REFERENCES core.merchant(id),
    storage_provider text NOT NULL DEFAULT 's3_cdn'
                     CHECK (storage_provider IN ('s3_cdn','local_nginx')),
    storage_key      text NOT NULL,          -- S3 object key ATAU path relatif local disk
    original_filename text NOT NULL,
    mime_type        text NOT NULL,
    size_bytes       bigint NOT NULL,
    checksum_sha256  text NOT NULL,          -- dedup: file sama -> skip upload ulang, tambah link baru
    status           text NOT NULL DEFAULT 'uploaded'
                     CHECK (status IN ('uploaded','ready','quarantined','deleted')),
    virus_scan_status text NOT NULL DEFAULT 'pending'
                     CHECK (virus_scan_status IN ('pending','clean','infected')),
    metadata         jsonb,                  -- variable-shape per mime_type: dimensi gambar, tag DICOM, jml halaman PDF, dst
    uploaded_by      uuid NOT NULL,
    created_at       timestamptz NOT NULL DEFAULT now(),
    updated_at       timestamptz NOT NULL DEFAULT now(),
    deleted_at       timestamptz,
    deleted_by       uuid,
    row_version      integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_media_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_media_merchant ON core.media (merchant_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_media_checksum ON core.media (merchant_id, checksum_sha256) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_media_touch BEFORE UPDATE ON core.media
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.media ENABLE ROW LEVEL SECURITY;
CREATE POLICY media_isolation ON core.media
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
CREATE TRIGGER trg_media_tenant_check BEFORE INSERT OR UPDATE ON core.media
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();

-- polymorphic link: 1 media bisa dipakai lebih dari 1 tempat, reference-count buat orphan cleanup
CREATE TABLE core.media_link (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    media_id     uuid NOT NULL REFERENCES core.media(id),
    owner_table  text NOT NULL,       -- "clinical_form", "person", "admission", dst — bukan FK (polymorphic)
    owner_id     uuid NOT NULL,
    purpose      text NOT NULL,       -- "patient_photo","consent_signature","lab_attachment", dst
    created_at   timestamptz NOT NULL DEFAULT now(),
    created_by   uuid NOT NULL,
    deleted_at   timestamptz,
    deleted_by   uuid,
    UNIQUE (media_id, owner_table, owner_id, purpose)
);
CREATE INDEX idx_media_link_media ON core.media_link (media_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_media_link_owner ON core.media_link (owner_table, owner_id) WHERE deleted_at IS NULL;
