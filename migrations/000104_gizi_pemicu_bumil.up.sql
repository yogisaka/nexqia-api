-- Plan 2026-11-16-gizi-maternal-1-be-hook Task 1 (spec
-- 2026-11-16-gizi-maternal-revisi-design §4.1–4.2, §10): template pemicu
-- bumil (LiLA/Hb/MST, stratifikasi dihitung server) + CHECK instrument
-- nutrition_screening_result diperluas dengan 'pemicu_bumil'.

INSERT INTO core.template (kind, code, name, description, scope, usage) VALUES
    ('clinical_form','ases_gizi_pemicu_bumil','Skrining Pemicu Risiko Bumil',
     'Pemicu risiko ibu hamil: LiLA (KEK <23,5 cm), Hb per trimester, MST; stratifikasi dihitung server (spec 2026-11-16-gizi-maternal)','platform','insert');

INSERT INTO core.template_clinical_form (template_id, form_type, category, sections)
SELECT t.id, 'ases_gizi_pemicu_bumil', 'gizi', '{
  "sections":[
    {"key":"lila_cm","label":"LiLA (lingkar lengan atas) — cm","kind":"number"},
    {"key":"hb_gdl","label":"Hb (hemoglobin) — g/dL","kind":"number"},
    {"key":"mgg","label":"Usia kehamilan saat pengukuran — minggu","kind":"number"},
    {"key":"interpretasi_pemicu","label":"Stratifikasi risiko bumil (diisi server)","kind":"select","options":[
      {"value":"rendah","label":"Rendah — tanpa pemicu"},
      {"value":"sedang","label":"Sedang — 1 pemicu"},
      {"value":"tinggi","label":"Tinggi — >=2 pemicu / anemia sedang-berat / KEK+anemia"}]}
  ]}'::jsonb
FROM core.template t
WHERE t.kind='clinical_form' AND t.code='ases_gizi_pemicu_bumil';

DO $$ BEGIN
  IF NOT EXISTS (SELECT 1 FROM core.template_clinical_form WHERE form_type='ases_gizi_pemicu_bumil') THEN
    RAISE EXCEPTION 'template ases_gizi_pemicu_bumil gagal dibuat (data acuan core.template hilang)';
  END IF;
END $$;

ALTER TABLE clinical.nutrition_screening_result
  DROP CONSTRAINT IF EXISTS nutrition_screening_result_instrument_check;
ALTER TABLE clinical.nutrition_screening_result
  ADD CONSTRAINT nutrition_screening_result_instrument_check
  CHECK (instrument IN ('mst','mnasf','rules','pemicu_bumil'));
