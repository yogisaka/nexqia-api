-- Korektif buat migration 000036: nyisipin core.permission baru (id
-- ...0025/...0026) buat code yang UDAH ADA di seed/001_core_seed.sql (id
-- ...0015/...0016). Idempoten by design — aman dijalanin baik di environment
-- yang punya duplikat maupun yang enggak. Lihat docs/design/plans/
-- 2026-09-24-fix-0a-0b-0c-review-findings.md Task 2 buat analisa lengkap.
DO $$
DECLARE
    v_canonical_id uuid;
    v_dup_id       uuid;
    v_code         text;
BEGIN
    FOR v_code, v_dup_id IN
        SELECT * FROM (VALUES
            ('core.user.manage', '00000000-0000-0000-0004-000000000025'::uuid),
            ('core.role.manage', '00000000-0000-0000-0004-000000000026'::uuid)
        ) AS t(code, dup_id)
    LOOP
        -- cari id LAIN (bukan id duplikat di atas) yang pegang code yang sama
        SELECT id INTO v_canonical_id
        FROM core.permission
        WHERE code = v_code AND id != v_dup_id
        LIMIT 1;

        IF v_canonical_id IS NULL THEN
            -- gak ada id lama sama sekali (kasus fresh-environment, seed
            -- belum pernah sukses buat row ini) — gak ada apa pun buat
            -- di-dedupe, id duplikat (kalau ada) otomatis jadi canonical.
            CONTINUE;
        END IF;

        IF NOT EXISTS (SELECT 1 FROM core.permission WHERE id = v_dup_id) THEN
            -- id duplikat gak pernah kebentuk di DB ini (ON CONFLICT (code)
            -- no-op waktu migration 000036 jalan) — gak ada apa pun buat
            -- dibersihin.
            CONTINUE;
        END IF;

        -- re-point grant role_permission yang kebetulan nunjuk ke id
        -- duplikat, biar gak ilang pas duplikatnya dihapus
        INSERT INTO core.role_permission (role_id, permission_id)
        SELECT role_id, v_canonical_id FROM core.role_permission WHERE permission_id = v_dup_id
        ON CONFLICT (role_id, permission_id) DO NOTHING;

        DELETE FROM core.role_permission WHERE permission_id = v_dup_id;
        DELETE FROM core.permission WHERE id = v_dup_id;
    END LOOP;
END $$;
