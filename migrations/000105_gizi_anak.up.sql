-- Plan 2026-11-18-anak-a1-1-be Task 1 (spec 2026-11-18-anak-gizi-a1-design
-- §3–§6, §8): profil gizi ke-6 'anak' (5–18 th), band status gizi
-- ref.growth_status_band usia 5–18 (label verbatim Permenkes 2/2020 —
-- docs/agent/ref/cppt/gizi/permenkes2-2020.txt; IMT/U 5–18 verbatim tabel
-- kategori, BB/U & TB/U 5–18 tidak punya tabel kategori terpisah → label
-- draft M1), template skrining STAMP (footer atribusi verbatim), CHECK
-- instrument nutrition_screening_result diperluas dengan 'stamp'.

-- 1. CHECK profile diperluas (nama constraint generated 000097 dipertahankan,
--    pola 000104:30-34 — DROP IF EXISTS lalu ADD nama sama).
ALTER TABLE clinical.nutrition_episode
  DROP CONSTRAINT IF EXISTS nutrition_episode_profile_check;
ALTER TABLE clinical.nutrition_episode
  ADD CONSTRAINT nutrition_episode_profile_check
  CHECK (profile IN ('dewasa','lansia','bumil','menyusui','balita','anak'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT IF EXISTS nutrition_profile_change_from_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_from_profile_check
  CHECK (from_profile IN ('dewasa','lansia','bumil','menyusui','balita','anak'));

ALTER TABLE clinical.nutrition_profile_change
  DROP CONSTRAINT IF EXISTS nutrition_profile_change_to_profile_check;
ALTER TABLE clinical.nutrition_profile_change
  ADD CONSTRAINT nutrition_profile_change_to_profile_check
  CHECK (to_profile IN ('dewasa','lansia','bumil','menyusui','balita','anak'));

-- 2. Band status gizi anak 5–18 (14 baris; low inklusif, high eksklusif;
--    cutoff Permenkes 2/2020 usia 5–18 — IMT/U verbatim; bbu: >+2 obesitas;
--    tbu: >+2 tinggi). Domain usia implisit (5–18) — dipilah trigger per
--    age_months (Task 2). Baris balita existing TIDAK berubah.
INSERT INTO ref.growth_status_band (indicator, basis, low, high, code, label, risk) VALUES
    ('bbu','z',NULL,-3,'gizi_buruk','Gizi buruk (kurus berat)','tinggi'),
    ('bbu','z',-3,-2,'gizi_kurang','Gizi kurang (kurus)','sedang'),
    ('bbu','z',-2,1,'gizi_baik','Gizi baik','rendah'),
    ('bbu','z',1,2,'gizi_lebih','Gizi lebih (gemuk)','sedang'),
    ('bbu','z',2,NULL,'obesitas','Obesitas','tinggi'),
    ('tbu','z',NULL,-3,'sangat_pendek','Sangat pendek (stunting berat)','tinggi'),
    ('tbu','z',-3,-2,'stunting','Pendek (stunting)','sedang'),
    ('tbu','z',-2,2,'normal','Normal','rendah'),
    ('tbu','z',2,NULL,'tinggi','Tinggi','rendah'),
    ('imtu','z',NULL,-3,'kurus_berat','Gizi buruk (severely thinness)','tinggi'),
    ('imtu','z',-3,-2,'kurus','Gizi kurang (thinness)','sedang'),
    ('imtu','z',-2,1,'normal','Gizi baik (normal)','rendah'),
    ('imtu','z',1,2,'gemuk','Gizi lebih (overweight)','sedang'),
    ('imtu','z',2,NULL,'obesitas','Obesitas (obese)','tinggi');

DO $$
DECLARE n integer;
BEGIN
    SELECT count(*) INTO n
    FROM (VALUES
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
    ) v(indicator, basis, low, high, code, label, risk)
    JOIN ref.growth_status_band b
      ON b.indicator = v.indicator AND b.basis = v.basis
     AND b.low IS NOT DISTINCT FROM v.low AND b.high IS NOT DISTINCT FROM v.high
     AND b.code = v.code AND b.label = v.label AND b.risk = v.risk;
    IF n <> 14 THEN
        RAISE EXCEPTION 'seed band anak 5-18 tidak lengkap: % dari 14', n;
    END IF;
END $$;

-- 3. Template skrining STAMP (pola 000104:6-22 / 000098:6-14). Step 1–2 radio
--    dari payload; step 3 (berat/tinggi vs grafik) dan risiko total dihitung
--    server (hook gizi_care, Task 3). Footer atribusi verbatim di description.
INSERT INTO core.template (kind, code, name, description, scope, usage) VALUES
    ('clinical_form','ases_gizi_stamp','Skrining Risiko Gizi Anak (STAMP)',
     'STAMP: langkah 1 diagnosis, langkah 2 asupan nutrisi, langkah 3 antropometri (server); risiko total >=4 tinggi, 2-3 sedang, 0-1 rendah. STAMP© — Screening Tool for the Assessment of Malnutrition in Paediatrics (stampscreeningtool.org)','platform','insert');

INSERT INTO core.template_clinical_form (template_id, form_type, category, sections)
SELECT t.id, 'ases_gizi_stamp', 'gizi', '{
  "sections":[
    {"key":"stamp_diagnosis","label":"Langkah 1 — Diagnosis","kind":"select","options":[
      {"value":"3","label":"Definitely — ada implikasi gizi pasti"},
      {"value":"2","label":"Possibly — mungkin ada implikasi"},
      {"value":"0","label":"No — tidak ada"}]},
    {"key":"stamp_asupan","label":"Langkah 2 — Asupan nutrisi","kind":"select","options":[
      {"value":"3","label":"Tidak ada asupan"},
      {"value":"2","label":"Asupan menurun / buruk"},
      {"value":"0","label":"Tidak berubah / baik"}]},
    {"key":"stamp_step3_sumber","label":"Langkah 3 — Berat & tinggi badan vs grafik","kind":"readonly","value":"Dihitung server dari antropometri terakhir"},
    {"key":"stamp_total","label":"Risiko total","kind":"readonly","value":"Dihitung server: >=4 tinggi, 2-3 sedang, 0-1 rendah"}
  ]}'::jsonb
FROM core.template t
WHERE t.kind='clinical_form' AND t.code='ases_gizi_stamp';

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM core.template_clinical_form WHERE form_type='ases_gizi_stamp') THEN
    RAISE EXCEPTION 'template ases_gizi_stamp gagal dibuat (data acuan core.template hilang)';
  END IF;
END $$;

-- 4. CHECK instrument diperluas dengan 'stamp'.
ALTER TABLE clinical.nutrition_screening_result
  DROP CONSTRAINT IF EXISTS nutrition_screening_result_instrument_check;
ALTER TABLE clinical.nutrition_screening_result
  ADD CONSTRAINT nutrition_screening_result_instrument_check
  CHECK (instrument IN ('mst','mnasf','rules','pemicu_bumil','stamp'));
