BEGIN;

CREATE TABLE core.document_type (
    code        text PRIMARY KEY,
    name        text NOT NULL,
    sort_order  integer NOT NULL DEFAULT 0
);

INSERT INTO core.document_type (code, name, sort_order) VALUES
    ('ktp', 'KTP', 10),
    ('kartu_keluarga', 'Kartu Keluarga', 20),
    ('kartu_bpjs', 'Kartu BPJS', 30),
    ('surat_rujukan', 'Surat Rujukan', 40),
    ('paspor', 'Paspor', 50),
    ('akta_lahir', 'Akta Kelahiran', 60),
    ('formulir_consent', 'Formulir Persetujuan', 70),
    ('lainnya', 'Lainnya', 90);

CREATE TABLE core.person_document (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    person_id     uuid NOT NULL REFERENCES core.person(id),
    media_id      uuid NOT NULL REFERENCES core.media(id),
    document_type text NOT NULL REFERENCES core.document_type(code),
    label         text NOT NULL,
    issued_date   date,
    expired_date  date,
    created_by    uuid NOT NULL REFERENCES core.app_user(id),
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    deleted_at    timestamptz,
    deleted_by    uuid REFERENCES core.app_user(id),
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_person_document_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL))
);
CREATE INDEX idx_person_document_person ON core.person_document (person_id) WHERE deleted_at IS NULL;
CREATE INDEX idx_person_document_media ON core.person_document (media_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_person_document_touch BEFORE UPDATE ON core.person_document
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE TRIGGER trg_person_document_tenant_check BEFORE INSERT OR UPDATE ON core.person_document
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE core.person_document ENABLE ROW LEVEL SECURITY;
CREATE POLICY person_document_isolation ON core.person_document
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

CREATE TABLE core.registration_document_requirement (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    service_type  text NOT NULL CHECK (service_type IN ('umum', 'bpjs', 'asuransi')),
    document_type text NOT NULL REFERENCES core.document_type(code),
    created_at    timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_registration_document_requirement UNIQUE (merchant_id, service_type, document_type)
);
CREATE INDEX idx_registration_document_requirement ON core.registration_document_requirement (merchant_id);
CREATE TRIGGER trg_registration_document_requirement_tenant_check BEFORE INSERT OR UPDATE ON core.registration_document_requirement
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE core.registration_document_requirement ENABLE ROW LEVEL SECURITY;
CREATE POLICY registration_document_requirement_isolation ON core.registration_document_requirement
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

INSERT INTO core.registration_document_requirement (company_id, merchant_id, service_type, document_type)
SELECT m.company_id, m.id, v.service_type, v.document_type
FROM core.merchant m
CROSS JOIN (VALUES
    ('umum', 'ktp'),
    ('bpjs', 'ktp'),
    ('bpjs', 'kartu_bpjs'),
    ('bpjs', 'surat_rujukan'),
    ('asuransi', 'ktp')
) AS v(service_type, document_type)
ON CONFLICT DO NOTHING;

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0005-000000000001', 'patient.document.view', 'Melihat dan mengunduh dokumen pasien', 'patient'),
    ('00000000-0000-0000-0005-000000000002', 'patient.document.manage', 'Unggah, ubah, dan hapus dokumen pasien', 'patient')
ON CONFLICT DO NOTHING;

INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('patient.document.view', 'patient.document.manage')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'patient.document.view'
WHERE tr.name IN ('Pendaftaran', 'Admin Klinik', 'Admin RS', 'Dokter', 'Dokter Spesialis', 'Perawat')
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'patient.document.manage'
WHERE tr.name IN ('Pendaftaran', 'Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;

UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

COMMIT;
