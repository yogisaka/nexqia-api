BEGIN;

-- Klasifikasi RS Permenkes 6/2026 Ps 12 (spec 2026-10-20-permenkes-6-2026
-- §6): ortogonal dengan facility_type; NULL kecuali rumah_sakit. Nilai
-- lama A/B/C/D (PMK 3/2020, dicabut) TIDAK dimigrasi — belum ada pemetaan
-- resmi; user mengisi manual.
ALTER TABLE core.merchant
    ADD COLUMN hospital_class text CONSTRAINT chk_merchant_hospital_class CHECK (
        hospital_class IS NULL
        OR (hospital_class IN ('paripurna', 'utama', 'madya', 'dasar')
            AND facility_type = 'rumah_sakit')
    );

COMMIT;
