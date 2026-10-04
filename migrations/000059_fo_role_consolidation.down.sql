-- Down best-effort, DEV-ONLY. Restore penuh jalur merge tidak mungkin tanpa data asli:
-- assignment yang sudah dipindah TIDAK dipulihkan (user re-assign manual).
-- Marker updated_by sentinel hanya ada di baris asal-FO Rajal.

-- 1. Rename balik hasil jalur 1/2: live 'Pendaftaran' bermarker -> FO Rajal.
UPDATE core.role
SET name = 'FO Rajal',
    description = 'Front Office Rawat Jalan - pendaftaran, check-in, dan antrian',
    updated_by = NULL
WHERE name = 'Pendaftaran'
  AND deleted_at IS NULL
  AND updated_by = '00000000-0000-0000-0000-000000000000'::uuid;

-- 2. Kembalikan nama baris soft-deleted yang digeser jalur 2
--    (SETELAH langkah 1 agar slot UNIQUE(company_id, name) bebas).
UPDATE core.role
SET name = regexp_replace(name, ' \(terhapus \d+\)$', '')
WHERE deleted_at IS NOT NULL
  AND name ~ ' \(terhapus \d+\)$';

-- 3. Jalur merge: aktifkan kembali FO Rajal bermarker; cabut DUA grant extra
--    dari role 'Pendaftaran' live di company yang sama (dua extra yang tidak
--    pernah diberikan template — aman dicabut untuk restore dev).
UPDATE core.role
SET deleted_at = NULL, deleted_by = NULL, updated_by = NULL
WHERE name = 'FO Rajal'
  AND deleted_at IS NOT NULL
  AND updated_by = '00000000-0000-0000-0000-000000000000'::uuid;

DELETE FROM core.role_permission rp
USING core.role t, core.role fo, core.permission p
WHERE rp.role_id = t.id
  AND t.name = 'Pendaftaran' AND t.deleted_at IS NULL
  AND t.company_id = fo.company_id
  AND fo.name = 'FO Rajal' AND fo.deleted_at IS NULL
  AND rp.permission_id = p.id
  AND p.code IN ('operations.schedule.manage', 'reporting.dashboard.view');
