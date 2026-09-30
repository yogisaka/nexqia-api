-- nexqia-api/migrations/000051_role_templates.up.sql
-- #7: generic template system + platform role templates per facility type
-- (spec 2026-09-30-role-templates-design §3). Templates are copied, never linked.

CREATE TABLE core.template (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    kind          text NOT NULL CHECK (kind IN ('role')),
    code          text NOT NULL,
    name          text NOT NULL,
    description   text,
    scope         text NOT NULL CHECK (scope IN ('platform', 'company', 'merchant', 'user')),
    company_id    uuid REFERENCES core.company(id),
    merchant_id   uuid REFERENCES core.merchant(id),
    owner_user_id uuid REFERENCES core.app_user(id),
    usage         text NOT NULL CHECK (usage IN ('copy', 'insert')),
    version       integer NOT NULL DEFAULT 1 CHECK (version >= 1),
    is_active     boolean NOT NULL DEFAULT true,
    created_at    timestamptz NOT NULL DEFAULT now(),
    updated_at    timestamptz NOT NULL DEFAULT now(),
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_template_scope CHECK (
        (scope = 'platform' AND company_id IS NULL AND merchant_id IS NULL AND owner_user_id IS NULL) OR
        (scope = 'company' AND company_id IS NOT NULL AND merchant_id IS NULL AND owner_user_id IS NULL) OR
        (scope = 'merchant' AND company_id IS NOT NULL AND merchant_id IS NOT NULL AND owner_user_id IS NULL) OR
        (scope = 'user' AND company_id IS NOT NULL AND merchant_id IS NULL AND owner_user_id IS NOT NULL)
    ),
    CONSTRAINT chk_template_role_usage CHECK (kind <> 'role' OR usage = 'copy'),
    CONSTRAINT uq_template_code UNIQUE NULLS NOT DISTINCT (company_id, merchant_id, owner_user_id, kind, code)
);
CREATE TRIGGER trg_template_touch BEFORE UPDATE ON core.template
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

CREATE TABLE core.template_role (
    id                      uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    template_id             uuid NOT NULL REFERENCES core.template(id) ON DELETE CASCADE,
    name                    text NOT NULL,
    description             text,
    requires_physician_data boolean NOT NULL DEFAULT false,
    sort_order              integer NOT NULL,
    UNIQUE (template_id, name)
);

CREATE TABLE core.template_role_permission (
    template_role_id uuid NOT NULL REFERENCES core.template_role(id) ON DELETE CASCADE,
    permission_id    uuid NOT NULL REFERENCES core.permission(id),
    PRIMARY KEY (template_role_id, permission_id)
);

CREATE TABLE core.template_application (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id        uuid NOT NULL REFERENCES core.company(id),
    template_id       uuid NOT NULL REFERENCES core.template(id),
    template_version  integer NOT NULL,
    applied_by        uuid REFERENCES core.app_user(id),
    platform_admin_id uuid,
    applied_at        timestamptz NOT NULL DEFAULT now(),
    roles_created     text[] NOT NULL DEFAULT '{}',
    roles_skipped     text[] NOT NULL DEFAULT '{}'
);
CREATE INDEX idx_template_application_company ON core.template_application (company_id, applied_at);

-- Tenants read platform rows + their own company's; child rows follow the header.
ALTER TABLE core.template ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_visibility ON core.template
    USING (company_id IS NULL OR company_id = current_setting('app.current_company_id')::uuid);
ALTER TABLE core.template_role ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_role_visibility ON core.template_role
    USING (EXISTS (SELECT 1 FROM core.template t WHERE t.id = template_id));
ALTER TABLE core.template_role_permission ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_role_permission_visibility ON core.template_role_permission
    USING (EXISTS (SELECT 1 FROM core.template_role r WHERE r.id = template_role_id));
ALTER TABLE core.template_application ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_application_isolation ON core.template_application
    USING (company_id = current_setting('app.current_company_id')::uuid);

-- Default privileges on schema core grant all four to app_runtime (000002). No endpoint
-- writes templates yet (platform rows change by migration only); applying only inserts history.
REVOKE INSERT, UPDATE, DELETE ON core.template, core.template_role, core.template_role_permission FROM app_runtime;
REVOKE UPDATE, DELETE ON core.template_application FROM app_runtime;

-- Seed: platform role templates (spec §3.2).
INSERT INTO core.template (kind, code, name, description, scope, usage) VALUES
    ('role', 'klinik_pratama', 'Klinik Pratama', 'Role dasar klinik pratama: admin, pendaftaran, dokter, perawat, kasir.', 'platform', 'copy'),
    ('role', 'klinik_utama', 'Klinik Utama', 'Role klinik utama, termasuk dokter spesialis.', 'platform', 'copy'),
    ('role', 'rumah_sakit', 'Rumah Sakit', 'Role rumah sakit, termasuk dokter spesialis dan manajemen.', 'platform', 'copy');

INSERT INTO core.template_role (template_id, name, description, requires_physician_data, sort_order)
SELECT t.id, r.name, r.description, r.physician, r.sort_order
FROM core.template t
JOIN (VALUES
    ('klinik_pratama', 'Admin Klinik', 'Kelola pengaturan klinik, user, role, data master, dan log audit.', false, 1),
    ('klinik_pratama', 'Pendaftaran', 'Pendaftaran pasien, kunjungan, dan loket antrian.', false, 2),
    ('klinik_pratama', 'Dokter', 'Pemeriksaan dan order klinis.', true, 3),
    ('klinik_pratama', 'Perawat', 'Melihat rekam medis pasien.', false, 4),
    ('klinik_pratama', 'Kasir', 'Membuat dan melihat tagihan.', false, 5),
    ('klinik_utama', 'Admin Klinik', 'Kelola pengaturan klinik, user, role, data master, dan log audit.', false, 1),
    ('klinik_utama', 'Pendaftaran', 'Pendaftaran pasien, kunjungan, dan loket antrian.', false, 2),
    ('klinik_utama', 'Dokter', 'Pemeriksaan dan order klinis.', true, 3),
    ('klinik_utama', 'Dokter Spesialis', 'Pemeriksaan dan order klinis spesialis.', true, 4),
    ('klinik_utama', 'Perawat', 'Melihat rekam medis pasien.', false, 5),
    ('klinik_utama', 'Kasir', 'Membuat dan melihat tagihan.', false, 6),
    ('rumah_sakit', 'Admin RS', 'Kelola pengaturan rumah sakit, user, role, data master, dan log audit.', false, 1),
    ('rumah_sakit', 'Pendaftaran', 'Pendaftaran pasien, kunjungan, dan loket antrian.', false, 2),
    ('rumah_sakit', 'Dokter', 'Pemeriksaan dan order klinis.', true, 3),
    ('rumah_sakit', 'Dokter Spesialis', 'Pemeriksaan dan order klinis spesialis.', true, 4),
    ('rumah_sakit', 'Perawat', 'Melihat rekam medis pasien.', false, 5),
    ('rumah_sakit', 'Kasir', 'Membuat dan melihat tagihan.', false, 6),
    ('rumah_sakit', 'Manajemen', 'Melihat dashboard laporan dan log audit.', false, 7)
) AS r(template_code, name, description, physician, sort_order) ON r.template_code = t.code
WHERE t.scope = 'platform' AND t.kind = 'role';

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN (VALUES
    ('Admin', 'core.merchant.manage'),
    ('Admin', 'core.department.manage'),
    ('Admin', 'core.physician.manage'),
    ('Admin', 'core.tariff.manage'),
    ('Admin', 'core.role.manage'),
    ('Admin', 'core.user.manage'),
    ('Admin', 'core.person.manage'),
    ('Admin', 'operations.schedule.manage'),
    ('Admin', 'audit.log.view'),
    ('Admin', 'reporting.dashboard.view'),
    ('Pendaftaran', 'core.person.manage'),
    ('Pendaftaran', 'operations.visit.manage'),
    ('Pendaftaran', 'operations.counter.manage'),
    ('Dokter', 'clinical.record.view'),
    ('Dokter', 'clinical.order.write'),
    ('Dokter Spesialis', 'clinical.record.view'),
    ('Dokter Spesialis', 'clinical.order.write'),
    ('Perawat', 'clinical.record.view'),
    ('Kasir', 'billing.invoice.create'),
    ('Kasir', 'billing.invoice.view'),
    ('Manajemen', 'reporting.dashboard.view'),
    ('Manajemen', 'audit.log.view')
) AS m(role_key, code)
    ON m.role_key = CASE WHEN tr.name IN ('Admin Klinik', 'Admin RS') THEN 'Admin' ELSE tr.name END
JOIN core.permission p ON p.code = m.code;

-- A permission code missing from the catalog would silently shrink a template: fail instead.
DO $$
DECLARE
    rec record;
    v_roles integer;
    v_perms integer;
BEGIN
    FOR rec IN SELECT * FROM (VALUES ('klinik_pratama', 5, 18), ('klinik_utama', 6, 20), ('rumah_sakit', 7, 22))
            AS v(code, roles, perms) LOOP
        SELECT count(DISTINCT tr.id), count(trp.permission_id) INTO v_roles, v_perms
        FROM core.template t
        JOIN core.template_role tr ON tr.template_id = t.id
        LEFT JOIN core.template_role_permission trp ON trp.template_role_id = tr.id
        WHERE t.code = rec.code AND t.scope = 'platform' AND t.kind = 'role';
        IF v_roles <> rec.roles OR v_perms <> rec.perms THEN
            RAISE EXCEPTION 'role template %: expected % roles / % permissions, got % / %',
                rec.code, rec.roles, rec.perms, v_roles, v_perms;
        END IF;
    END LOOP;
END;
$$;
