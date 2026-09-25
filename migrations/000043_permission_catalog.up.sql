-- The permission catalog lived only in seed/001_core_seed.sql (demo data, not
-- run in production). Without it a newly registered Owner gets only the few
-- codes earlier migrations inserted and cannot even create a merchant. Same ids,
-- codes and descriptions as the seed so both stay consistent; ON CONFLICT DO
-- NOTHING (no target) skips a row whose id OR code already exists.
INSERT INTO core.permission (id, code, description, module) VALUES
    ('00000000-0000-0000-0004-000000000001', 'billing.invoice.create', 'Membuat invoice tagihan', 'billing'),
    ('00000000-0000-0000-0004-000000000002', 'billing.invoice.view', 'Melihat invoice tagihan', 'billing'),
    ('00000000-0000-0000-0004-000000000003', 'clinical.order.write', 'Membuat order tindakan klinis', 'clinical'),
    ('00000000-0000-0000-0004-000000000004', 'clinical.record.view', 'Melihat rekam medis pasien', 'clinical'),
    ('00000000-0000-0000-0004-000000000005', 'master.person.manage', 'Kelola data master person', 'master'),
    ('00000000-0000-0000-0004-000000000006', 'master.service_item.manage', 'Kelola katalog layanan dan tarif', 'master'),
    ('00000000-0000-0000-0004-000000000007', 'rbac.user.manage', 'Kelola user dan akses', 'rbac'),
    ('00000000-0000-0000-0004-000000000008', 'rbac.role.manage', 'Kelola role dan permission', 'rbac'),
    ('00000000-0000-0000-0004-000000000009', 'reporting.dashboard.view', 'Melihat dashboard laporan', 'reporting'),
    ('00000000-0000-0000-0004-000000000010', 'procurement.po.create', 'Membuat purchase order', 'procurement'),
    ('00000000-0000-0000-0004-000000000011', 'notification.manage', 'Kelola notifikasi inbox', 'notification'),
    ('00000000-0000-0000-0004-000000000012', 'audit.log.view', 'Melihat log audit dan akses', 'audit'),
    ('00000000-0000-0000-0004-000000000013', 'core.company.manage', 'Kelola company (platform-admin)', 'core'),
    ('00000000-0000-0000-0004-000000000014', 'core.merchant.manage', 'Kelola merchant', 'core'),
    ('00000000-0000-0000-0004-000000000015', 'core.user.manage', 'Kelola user dan akses', 'core'),
    ('00000000-0000-0000-0004-000000000016', 'core.role.manage', 'Kelola role dan permission', 'core'),
    ('00000000-0000-0000-0004-000000000017', 'core.person.manage', 'Kelola data master person', 'core'),
    ('00000000-0000-0000-0004-000000000018', 'core.department.manage', 'Kelola master data poli/departemen', 'core'),
    ('00000000-0000-0000-0004-000000000019', 'core.tariff.manage', 'Kelola katalog layanan dan tarif', 'core'),
    ('00000000-0000-0000-0004-000000000020', 'core.physician.manage', 'Kelola data dokter', 'core'),
    ('00000000-0000-0000-0004-000000000021', 'operations.schedule.manage', 'Kelola jadwal praktik dokter', 'operations'),
    ('00000000-0000-0000-0004-000000000022', 'operations.counter.manage', 'Kelola loket dan panggil antrian', 'operations'),
    ('00000000-0000-0000-0004-000000000023', 'operations.visit.manage', 'Kelola kunjungan rajal dan antrian', 'operations')
ON CONFLICT DO NOTHING;

-- Owners registered before this migration only received the codes that existed
-- at registration time. Give every system Owner role the full catalog — same
-- rule as RegisterHandler: everything except the platform-wide core.company.manage.
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id
FROM core.role r CROSS JOIN core.permission p
WHERE r.name = 'Owner' AND r.is_system AND r.deleted_at IS NULL
  AND p.code <> 'core.company.manage'
ON CONFLICT DO NOTHING;