-- Restores the catalog rows only; the removed grants are not restored (they
-- were moved to the core.* equivalents by the up migration).
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000005', 'master.person.manage', 'Kelola data master person', 'master'),
    ('00000000-0000-0000-0004-000000000006', 'master.service_item.manage', 'Kelola katalog layanan dan tarif', 'master'),
    ('00000000-0000-0000-0004-000000000007', 'rbac.user.manage', 'Kelola user dan akses', 'rbac'),
    ('00000000-0000-0000-0004-000000000008', 'rbac.role.manage', 'Kelola role dan permission', 'rbac'),
    ('00000000-0000-0000-0004-000000000010', 'procurement.po.create', 'Membuat purchase order', 'procurement')
ON CONFLICT DO NOTHING;