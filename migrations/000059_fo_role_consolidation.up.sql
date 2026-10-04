-- Konsolidasi role FO Rajal -> Pendaftaran.
-- Konsolidasi role FO Rajal -> Pendaftaran sesuai keputusan user 2026-10-04 (§4.1 dokumen desain dashboard FO).
-- Marker updated_by sentinel HANYA pada baris asal-FO Rajal (hasil rename jalur 1/2,
-- soft-delete jalur 3). Role tujuan merge TIDAK ditandai — down migration
-- membedakannya secara struktural (lihat down.sql).
-- Urutan wajib: aside (jalur 2) -> merge (jalur 3) -> rename (jalur 1) -> grant -> check.

-- Jalur 2: geser baris 'Pendaftaran' soft-deleted yang menghalangi slot UNIQUE
-- di company yang punya FO Rajal live dan TIDAK punya Pendaftaran live.
UPDATE core.role
SET name = name || ' (terhapus ' || to_char(now(), 'YYYYMMDD') || ')'
WHERE deleted_at IS NOT NULL
  AND name = 'Pendaftaran'
  AND company_id IN (
      SELECT company_id FROM core.role
      WHERE name = 'FO Rajal' AND deleted_at IS NULL
  )
  AND NOT EXISTS (
      SELECT 1 FROM core.role t
      WHERE t.company_id = core.role.company_id
        AND t.name = 'Pendaftaran' AND t.deleted_at IS NULL
  );

-- Jalur 3: company punya 'Pendaftaran' live -> merge (FO Rajal tidak di-rename).

-- 3a. Pindahkan assignment user_merchant_role.
INSERT INTO core.user_merchant_role (id, user_id, merchant_id, role_id)
SELECT uuid_generate_v7(), umr.user_id, umr.merchant_id, target.id
FROM core.user_merchant_role umr
JOIN core.role fo ON fo.id = umr.role_id
     AND fo.name = 'FO Rajal' AND fo.deleted_at IS NULL
JOIN core.role target ON target.company_id = fo.company_id
     AND target.name = 'Pendaftaran' AND target.deleted_at IS NULL
ON CONFLICT (user_id, merchant_id, role_id) DO NOTHING;

DELETE FROM core.user_merchant_role umr
USING core.role fo
WHERE umr.role_id = fo.id
  AND fo.name = 'FO Rajal' AND fo.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM core.role t
      WHERE t.company_id = fo.company_id
        AND t.name = 'Pendaftaran' AND t.deleted_at IS NULL
  );

-- 3b. Pindahkan assignment user_company_role.
INSERT INTO core.user_company_role (id, user_id, company_id, role_id)
SELECT uuid_generate_v7(), ucr.user_id, ucr.company_id, target.id
FROM core.user_company_role ucr
JOIN core.role fo ON fo.id = ucr.role_id
     AND fo.name = 'FO Rajal' AND fo.deleted_at IS NULL
JOIN core.role target ON target.company_id = fo.company_id
     AND target.name = 'Pendaftaran' AND target.deleted_at IS NULL
ON CONFLICT (user_id, company_id, role_id) DO NOTHING;

DELETE FROM core.user_company_role ucr
USING core.role fo
WHERE ucr.role_id = fo.id
  AND fo.name = 'FO Rajal' AND fo.deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM core.role t
      WHERE t.company_id = fo.company_id
        AND t.name = 'Pendaftaran' AND t.deleted_at IS NULL
  );

-- 3c. Netralkan sesi dengan role aktif FO Rajal di company merge
-- (pemilik sesi memilih role lagi di refresh berikutnya).
UPDATE core.refresh_token rt
SET active_role_id = NULL
WHERE rt.active_role_id IS NOT NULL
  AND EXISTS (
      SELECT 1 FROM core.role fo
      WHERE fo.id = rt.active_role_id
        AND fo.name = 'FO Rajal' AND fo.deleted_at IS NULL
        AND EXISTS (
            SELECT 1 FROM core.role t
            WHERE t.company_id = fo.company_id
              AND t.name = 'Pendaftaran' AND t.deleted_at IS NULL
        )
  );

-- 3d. Soft-delete FO Rajal di company merge (marker pada baris asal-FO Rajal).
UPDATE core.role
SET deleted_at = now(),
    deleted_by = '00000000-0000-0000-0000-000000000000'::uuid,
    updated_by = '00000000-0000-0000-0000-000000000000'::uuid
WHERE name = 'FO Rajal' AND deleted_at IS NULL
  AND EXISTS (
      SELECT 1 FROM core.role t
      WHERE t.company_id = core.role.company_id
        AND t.name = 'Pendaftaran' AND t.deleted_at IS NULL
  );

-- Jalur 1: rename sisa FO Rajal live (jalur 2 sudah membebaskan slot UNIQUE;
-- jalur 3 sudah memindahkan company merge ke soft-delete).
UPDATE core.role
SET name = 'Pendaftaran',
    description = 'Pendaftaran pasien, kunjungan, dan loket antrian.',
    updated_by = '00000000-0000-0000-0000-000000000000'::uuid
WHERE name = 'FO Rajal' AND deleted_at IS NULL;

-- Langkah 4: lengkapi 5 permission HANYA pada target konsolidasi:
-- (i) live 'Pendaftaran' bermarker (hasil rename jalur 1/2), UNION
-- (ii) live 'Pendaftaran' di company yang punya FO Rajal soft-deleted bermarker (tujuan merge jalur 3).
-- Role 'Pendaftaran' hasil template di company lain TIDAK disentuh.
INSERT INTO core.role_permission (role_id, permission_id)
SELECT r.id, p.id
FROM core.role r
JOIN core.permission p ON p.code IN (
    'core.person.manage',
    'operations.visit.manage',
    'operations.counter.manage',
    'operations.schedule.manage',
    'reporting.dashboard.view'
)
WHERE r.deleted_at IS NULL
  AND r.name = 'Pendaftaran'
  AND (
      r.updated_by = '00000000-0000-0000-0000-000000000000'::uuid
      OR EXISTS (
          SELECT 1 FROM core.role fo
          WHERE fo.company_id = r.company_id
            AND fo.name = 'FO Rajal'
            AND fo.deleted_at IS NOT NULL
            AND fo.updated_by = '00000000-0000-0000-0000-000000000000'::uuid
      )
  )
ON CONFLICT (role_id, permission_id) DO NOTHING;

-- Gagal keras bila masih ada FO Rajal live.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM core.role WHERE name = 'FO Rajal' AND deleted_at IS NULL) THEN
        RAISE EXCEPTION '000059: masih ada role live FO Rajal setelah konsolidasi';
    END IF;
END $$;
