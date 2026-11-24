BEGIN;

ALTER TABLE ref.immunization_schedule
  ADD COLUMN gender text NULL CHECK (gender IN ('perempuan')),
  ADD COLUMN area_khusus text NULL CHECK (area_khusus IN ('je','rv','ipv'));

INSERT INTO ref.immunization_schedule
  (antigen_slug, label, dosis_ke, kategori, usia_min_bln, usia_maks_bln,
   interval_hari_dari_dosis_sebelumnya, masa_perlindungan_tahun, gender, area_khusus,
   source_name, source_version, source_url, downloaded_at)
VALUES
  ('hb','Hepatitis B (HB0)',1,'dasar',0,NULL,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('bcg','BCG',1,'dasar',1,11,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('opv','Polio tetes (bOPV)',1,'dasar',1,59,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('opv','Polio tetes (bOPV)',2,'dasar',2,59,28,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('opv','Polio tetes (bOPV)',3,'dasar',3,59,28,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('opv','Polio tetes (bOPV)',4,'dasar',4,59,28,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('dpt_hb_hib','DPT-HB-Hib',1,'dasar',2,59,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('dpt_hb_hib','DPT-HB-Hib',2,'dasar',3,59,28,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('dpt_hb_hib','DPT-HB-Hib',3,'dasar',4,59,28,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('dpt_hb_hib','DPT-HB-Hib',4,'lanjutan',18,59,365,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('pcv','PCV',1,'dasar',2,59,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('pcv','PCV',2,'dasar',3,59,28,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('pcv','PCV',3,'dasar',12,59,56,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('rv','Rotavirus (RV)',1,'dasar',2,7,NULL,NULL,NULL,'rv','Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('rv','Rotavirus (RV)',2,'dasar',3,7,28,NULL,NULL,'rv','Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('rv','Rotavirus (RV)',3,'dasar',4,7,28,NULL,NULL,'rv','Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('ipv','Polio Suntik (IPV)',1,'dasar',4,59,NULL,NULL,NULL,'ipv','Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('ipv','Polio Suntik (IPV)',2,'dasar',9,59,120,NULL,NULL,'ipv','Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('mr','Campak Rubela (MR)',1,'dasar',9,59,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('mr','Campak Rubela (MR)',2,'dasar',18,59,180,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('je','JE',1,'dasar',10,59,NULL,NULL,NULL,'je','Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('dt','DT',1,'bias',84,NULL,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('td','Td',1,'bias',96,144,NULL,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('td','Td',2,'bias',132,144,365,NULL,NULL,NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP),
  ('hpv','HPV',1,'bias',132,180,NULL,NULL,'perempuan',NULL,'Kemenkes RI','kia-2024-kmk35-2025','https://kesprimkom.kemkes.go.id/assets/uploads/contents/others/Buku_KIA_2024.pdf',CURRENT_TIMESTAMP)
ON CONFLICT (antigen_slug, dosis_ke, kategori, source_version) DO NOTHING;
GRANT SELECT ON ref.immunization_schedule TO app_runtime;

COMMIT;
