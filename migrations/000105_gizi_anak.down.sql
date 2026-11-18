-- Plan 2026-11-18-anak-a1-1-be Task 1 (spec 2026-11-18-anak-gizi-a1-design
-- §3–§6, §8): DOWN migration 000105_gizi_anak. CHECK instrument sengaja TIDAK
-- direstore ke 4 nilai (pola 000104: baris 'stamp' historis dipertahankan —
-- restore akan gagal validasi). CHECK profile dikembalikan ke 5 nilai HANYA
-- bila tidak ada baris 'anak'.

DELETE FROM core.template_clinical_form WHERE form_type='ases_gizi_stamp';
DELETE FROM core.template WHERE kind='clinical_form' AND code='ases_gizi_stamp';

-- Band anak 5–18: hapus SATU baris per tuple anak. Tiga tuple identik dengan
-- baris balita ('Gizi baik' bbu -2/+1, 'Sangat pendek (stunting berat)' tbu
-- NULL/-3, 'Tinggi' tbu +2/NULL) — hapus satu instance (ctid), bukan semua,
-- agar baris balita tetap utuh.
DO $$
DECLARE v record;
BEGIN
    FOR v IN SELECT * FROM (VALUES
        ('bbu','z',NULL::numeric,-3::numeric,'gizi_buruk','Gizi buruk (kurus berat)','tinggi'),
        ('bbu','z',-3::numeric,-2::numeric,'gizi_kurang','Gizi kurang (kurus)','sedang'),
        ('bbu','z',-2::numeric,1::numeric,'gizi_baik','Gizi baik','rendah'),
        ('bbu','z',1::numeric,2::numeric,'gizi_lebih','Gizi lebih (gemuk)','sedang'),
        ('bbu','z',2::numeric,NULL::numeric,'obesitas','Obesitas','tinggi'),
        ('tbu','z',NULL::numeric,-3::numeric,'sangat_pendek','Sangat pendek (stunting berat)','tinggi'),
        ('tbu','z',-3::numeric,-2::numeric,'stunting','Pendek (stunting)','sedang'),
        ('tbu','z',-2::numeric,2::numeric,'normal','Normal','rendah'),
        ('tbu','z',2::numeric,NULL::numeric,'tinggi','Tinggi','rendah'),
        ('imtu','z',NULL::numeric,-3::numeric,'kurus_berat','Gizi buruk (severely thinness)','tinggi'),
        ('imtu','z',-3::numeric,-2::numeric,'kurus','Gizi kurang (thinness)','sedang'),
        ('imtu','z',-2::numeric,1::numeric,'normal','Gizi baik (normal)','rendah'),
        ('imtu','z',1::numeric,2::numeric,'gemuk','Gizi lebih (overweight)','sedang'),
        ('imtu','z',2::numeric,NULL::numeric,'obesitas','Obesitas (obese)','tinggi')
    ) t(indicator, basis, low, high, code, label, risk)
    LOOP
        DELETE FROM ref.growth_status_band b
        WHERE b.ctid = (
            SELECT c.ctid FROM ref.growth_status_band c
            WHERE c.indicator = v.indicator AND c.basis = v.basis
              AND c.low IS NOT DISTINCT FROM v.low AND c.high IS NOT DISTINCT FROM v.high
              AND c.code = v.code AND c.label = v.label AND c.risk = v.risk
            LIMIT 1);
    END LOOP;
END $$;

-- CHECK profile kembali ke 5 nilai hanya bila tidak ada baris 'anak'.
DO $$
BEGIN
    IF EXISTS (SELECT 1 FROM clinical.nutrition_episode WHERE profile='anak')
       OR EXISTS (SELECT 1 FROM clinical.nutrition_profile_change
                  WHERE from_profile='anak' OR to_profile='anak') THEN
        RAISE EXCEPTION 'masih ada baris profil anak — CHECK profile tidak bisa dikembalikan ke 5 nilai';
    END IF;
END $$;

ALTER TABLE clinical.nutrition_episode
  DROP CONSTRAINT nutrition_episode_profile_check;
ALTER TABLE clinical.nutrition_episode
  ADD CONSTRAINT nutrition_episode_profile_check
  CHECK (profile IN ('dewasa','lansia','bumil','menyusui','balita'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT nutrition_profile_change_from_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_from_profile_check
  CHECK (from_profile IN ('dewasa','lansia','bumil','menyusui','balita'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT nutrition_profile_change_to_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_to_profile_check
  CHECK (to_profile IN ('dewasa','lansia','bumil','menyusui','balita'));
