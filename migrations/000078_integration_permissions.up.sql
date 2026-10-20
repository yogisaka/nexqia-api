BEGIN;

INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0005-000000000004', 'integration.status.view',
     'Melihat status sinkronisasi integrasi (SATUSEHAT)', 'integration'),
    ('00000000-0000-0000-0005-000000000005', 'integration.config.manage',
     'Mengelola kredensial dan sinkronisasi integrasi', 'integration')
ON CONFLICT DO NOTHING;

INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id FROM core.role r
JOIN core.permission p ON p.code IN ('integration.status.view', 'integration.config.manage')
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'integration.status.view'
WHERE tr.name IN ('Pendaftaran', 'Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;

INSERT INTO core.template_role_permission (template_role_id, permission_id)
SELECT tr.id, p.id
FROM core.template_role tr
JOIN core.template t ON t.id = tr.template_id AND t.scope = 'platform' AND t.kind = 'role'
JOIN core.permission p ON p.code = 'integration.config.manage'
WHERE tr.name IN ('Admin Klinik', 'Admin RS')
ON CONFLICT DO NOTHING;

UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

COMMIT;
