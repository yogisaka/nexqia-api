BEGIN;

-- Insiden keselamatan pasien B1 report-only (spec
-- 2026-10-20-permenkes-6-2026 §4 Fase B / §6): WHO MIM PS domain "what
-- happened"; kronologi append-only; katalog data-driven NCC MERP A-I
-- dipetakan outcome ICPS + 10 kategori jenis, overridable per merchant.
CREATE SCHEMA quality;
GRANT USAGE ON SCHEMA quality TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA quality GRANT SELECT, INSERT, UPDATE, DELETE ON TABLES TO app_runtime;
ALTER DEFAULT PRIVILEGES IN SCHEMA quality GRANT USAGE, SELECT ON SEQUENCES TO app_runtime;

CREATE TABLE quality.incident_grading (
    code        text PRIMARY KEY,
    name        text NOT NULL,
    outcome     text NOT NULL CHECK (outcome IN ('near_miss_no_harm', 'adverse_event', 'sentinel')),
    description text NOT NULL,
    legal_ref   text NOT NULL,
    sort_order  integer NOT NULL DEFAULT 0
);
REVOKE INSERT, UPDATE, DELETE ON quality.incident_grading FROM app_runtime;

INSERT INTO quality.incident_grading (code, name, outcome, description, legal_ref, sort_order) VALUES
    ('A', 'Tidak sampai ke pasien (near miss)', 'near_miss_no_harm', 'Kejadian berbahaya tertangani sebelum mencapai pasien', 'NCC MERP Index (nccmerp.org), kategori A; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 10),
    ('B', 'Sampai ke pasien, tanpa bahaya', 'near_miss_no_harm', 'Kejadian sampai ke pasien tetapi tidak menimbulkan bahaya', 'NCC MERP Index (nccmerp.org), kategori B; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 20),
    ('C', 'Tanpa bahaya, perlu pemantauan', 'near_miss_no_harm', 'Sampai ke pasien, tidak menimbulkan bahaya, tetapi memerlukan pemantauan', 'NCC MERP Index (nccmerp.org), kategori C; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 30),
    ('D', 'Tanpa bahaya, perlu tindakan', 'near_miss_no_harm', 'Sampai ke pasien, tidak menimbulkan bahaya, tetapi memerlukan tindakan', 'NCC MERP Index (nccmerp.org), kategori D; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 40),
    ('E', 'Bahaya sementara, perlu tindakan', 'adverse_event', 'Menimbulkan bahaya sementara pada pasien dan memerlukan tindakan', 'NCC MERP Index (nccmerp.org), kategori E; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 50),
    ('F', 'Bahaya sementara, perlu rawat inap', 'adverse_event', 'Menimbulkan bahaya sementara yang memerlukan rawat inap awal atau perpanjangan rawat inap', 'NCC MERP Index (nccmerp.org), kategori F; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 60),
    ('G', 'Bahaya permanen', 'adverse_event', 'Menimbulkan bahaya permanen pada pasien', 'NCC MERP Index (nccmerp.org), kategori G; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 70),
    ('H', 'Perlu tindakan menyelamatkan nyawa', 'adverse_event', 'Diperlukan tindakan penting untuk mempertahankan nyawa pasien', 'NCC MERP Index (nccmerp.org), kategori H; peta outcome WHO ICPS — spec 2026-10-20-permenkes-6-2026 §6', 80),
    ('I', 'Kematian pasien (insiden sentinel)', 'sentinel', 'Insiden yang berkontribusi pada atau mengakibatkan kematian pasien', 'NCC MERP Index (nccmerp.org), kategori I; peta outcome WHO ICPS / definisi sentinel KNKP — spec 2026-10-20-permenkes-6-2026 §6', 90);

CREATE TABLE quality.incident_type (
    code      text PRIMARY KEY,
    name      text NOT NULL,
    legal_ref text NOT NULL,
    sort_order integer NOT NULL DEFAULT 0
);
REVOKE INSERT, UPDATE, DELETE ON quality.incident_type FROM app_runtime;

INSERT INTO quality.incident_type (code, name, legal_ref, sort_order) VALUES
    ('klinis_prosedur', 'Klinis & Prosedur', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 10),
    ('medikasi', 'Medikasi', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 20),
    ('infeksi_higiene', 'Infeksi & Higiene', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 30),
    ('jatuh_pasien', 'Jatuh Pasien', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 40),
    ('dokumentasi_identifikasi', 'Dokumentasi & Identifikasi', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 50),
    ('alat_kesehatan', 'Alat Kesehatan', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 60),
    ('darah_produk_biologi', 'Darah & Produk Biologi', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 70),
    ('komunikasi_serah_terima', 'Komunikasi & Serah Terima', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 80),
    ('kebijakan_administratif', 'Kebijakan & Administratif', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 90),
    ('lainnya', 'Lainnya', 'WHO MIM PS; SNKARS/KNKP (riset 2026-10-20) — spec 2026-10-20-permenkes-6-2026 §4 Fase B', 100);

-- Override katalog jenis per merchant (kontrak §6 "overridable per
-- merchant"): ubah label / sembunyikan kategori. Baris global (seed) tetap
-- read-only bagi app_runtime; katalog global tidak ber-RLS (pola
-- core.document_type 000073:3-7) karena tanpa kolom tenant.
CREATE TABLE quality.incident_type_override (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    incident_type text NOT NULL REFERENCES quality.incident_type(code),
    name          text,
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT uq_incident_type_override UNIQUE (merchant_id, incident_type)
);
CREATE TRIGGER trg_incident_type_override_touch BEFORE UPDATE ON quality.incident_type_override
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE TRIGGER trg_incident_type_override_tenant_check BEFORE INSERT OR UPDATE ON quality.incident_type_override
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE quality.incident_type_override ENABLE ROW LEVEL SECURITY;
CREATE POLICY incident_type_override_isolation ON quality.incident_type_override
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

CREATE TABLE quality.incident (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id     uuid NOT NULL REFERENCES core.company(id),
    merchant_id    uuid NOT NULL REFERENCES core.merchant(id),
    incident_type  text NOT NULL REFERENCES quality.incident_type(code),
    grading        text NOT NULL REFERENCES quality.incident_grading(code),
    occurred_at    timestamptz NOT NULL,
    location       text NOT NULL CONSTRAINT chk_incident_location CHECK (btrim(location) <> ''),
    narrative      text NOT NULL CONSTRAINT chk_incident_narrative CHECK (btrim(narrative) <> ''),
    patient_impact text NOT NULL CONSTRAINT chk_incident_patient_impact CHECK (btrim(patient_impact) <> ''),
    person_id      uuid REFERENCES core.person(id),
    admission_id   uuid REFERENCES operations.admission(id),
    reported_by    uuid NOT NULL REFERENCES core.app_user(id),
    anonymous      boolean NOT NULL DEFAULT false,
    status         text NOT NULL DEFAULT 'dilaporkan'
                   CONSTRAINT chk_incident_status CHECK (status IN ('dilaporkan', 'feedback', 'closed')),
    created_at     timestamptz NOT NULL DEFAULT now(),
    updated_at     timestamptz NOT NULL DEFAULT now(),
    row_version    integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_incident_merchant_status ON quality.incident (merchant_id, status);
CREATE INDEX idx_incident_person ON quality.incident (person_id);
CREATE TRIGGER trg_incident_touch BEFORE UPDATE ON quality.incident
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
CREATE TRIGGER trg_incident_tenant_check BEFORE INSERT OR UPDATE ON quality.incident
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE quality.incident ENABLE ROW LEVEL SECURITY;
CREATE POLICY incident_isolation ON quality.incident
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

-- Kronologi append-only (koreksi = entri baru; kind feedback/status hanya
-- ditulis oleh PATCH /status, report oleh POST /incidents).
CREATE TABLE quality.incident_entry (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    merchant_id uuid NOT NULL REFERENCES core.merchant(id),
    incident_id uuid NOT NULL REFERENCES quality.incident(id),
    kind        text NOT NULL CHECK (kind IN ('report', 'correction', 'comment', 'feedback', 'status')),
    body        text NOT NULL CONSTRAINT chk_incident_entry_body CHECK (btrim(body) <> ''),
    created_by  uuid NOT NULL REFERENCES core.app_user(id),
    created_at  timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX idx_incident_entry_incident ON quality.incident_entry (incident_id);
CREATE TRIGGER trg_incident_entry_tenant_check BEFORE INSERT OR UPDATE ON quality.incident_entry
    FOR EACH ROW EXECUTE FUNCTION trg_check_tenant_consistency();
ALTER TABLE quality.incident_entry ENABLE ROW LEVEL SECURITY;
CREATE POLICY incident_entry_isolation ON quality.incident_entry
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
REVOKE UPDATE, DELETE ON quality.incident_entry FROM app_runtime;

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0006-000000000001', 'quality.incident.view', 'Melihat insiden keselamatan pasien', 'quality'),
    ('00000000-0000-0000-0006-000000000002', 'quality.incident.manage', 'Menindaklanjuti dan menutup insiden keselamatan pasien', 'quality'),
    ('00000000-0000-0000-0006-000000000003', 'quality.incident.report', 'Melaporkan insiden keselamatan pasien', 'quality')
ON CONFLICT DO NOTHING;

INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('quality.incident.view', 'quality.incident.manage', 'quality.incident.report')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code IN ('quality.incident.view', 'quality.incident.report')
WHERE tr.name IN ('Pendaftaran', 'Admin Klinik', 'Admin RS', 'Dokter', 'Dokter Spesialis', 'Perawat')
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'quality.incident.manage'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;

UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

COMMIT;
