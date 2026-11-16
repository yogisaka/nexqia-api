-- Plan 2026-11-16-gizi-maternal-1-be-hook Task 1 (spec §10): DOWN hanya
-- menghapus template; CHECK instrument sengaja TIDAK direstore ke 3 nilai
-- karena baris nutrition_screening_result 'pemicu_bumil' historis
-- dipertahankan — restore akan gagal validasi.

DELETE FROM core.template_clinical_form WHERE form_type='ases_gizi_pemicu_bumil';
DELETE FROM core.template WHERE kind='clinical_form' AND code='ases_gizi_pemicu_bumil';
