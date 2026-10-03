-- nexqia-api/migrations/000054_department_templates.up.sql
-- #26 department templates + external code mapping (spec 2026-10-01-department-templates-design §3-§4).

ALTER TABLE core.template DROP CONSTRAINT template_kind_check;
ALTER TABLE core.template ADD CONSTRAINT template_kind_check CHECK (kind IN ('role', 'department'));
ALTER TABLE core.template ADD CONSTRAINT chk_template_department_usage CHECK (kind <> 'department' OR usage = 'copy');

CREATE TABLE core.template_department (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    template_id    uuid NOT NULL REFERENCES core.template(id) ON DELETE CASCADE,
    code           text NOT NULL,
    name           text NOT NULL,
    specialty_code text,
    sort_order     integer NOT NULL,
    UNIQUE (template_id, code)
);

CREATE TABLE core.template_department_code_map (
    template_department_id uuid NOT NULL REFERENCES core.template_department(id) ON DELETE CASCADE,
    system                 text NOT NULL,
    code                   text NOT NULL,
    PRIMARY KEY (template_department_id, system)
);

ALTER TABLE core.template_department ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_department_visibility ON core.template_department
    USING (EXISTS (SELECT 1 FROM core.template t WHERE t.id = template_id));
ALTER TABLE core.template_department_code_map ENABLE ROW LEVEL SECURITY;
CREATE POLICY template_department_code_map_visibility ON core.template_department_code_map
    USING (EXISTS (SELECT 1 FROM core.template_department d WHERE d.id = template_department_id));
REVOKE INSERT, UPDATE, DELETE ON core.template_department, core.template_department_code_map FROM app_runtime;

-- Per-department codes in external systems (BPJS VClaim/PCare, SATUSEHAT, insurers).
CREATE TABLE core.department_code_map (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    merchant_id   uuid NOT NULL REFERENCES core.merchant(id),
    department_id uuid NOT NULL REFERENCES core.department(id) ON DELETE CASCADE,
    system        text NOT NULL CHECK (system ~ '^[a-z0-9][a-z0-9:._-]*$'),
    code          text NOT NULL CHECK (btrim(code) <> ''),
    display       text,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,   -- trg_touch_row() increments it
    UNIQUE (department_id, system)
);
CREATE INDEX idx_department_code_map_lookup ON core.department_code_map (merchant_id, system, code);

CREATE FUNCTION core.trg_fill_tenant_department_code_map() RETURNS trigger
LANGUAGE plpgsql AS $$
BEGIN
    NEW.company_id  := (SELECT d.company_id  FROM core.department d WHERE d.id = NEW.department_id);
    NEW.merchant_id := (SELECT d.merchant_id FROM core.department d WHERE d.id = NEW.department_id);
    RETURN NEW;
END;
$$;
CREATE TRIGGER trg_00_fill_tenant BEFORE INSERT OR UPDATE ON core.department_code_map
    FOR EACH ROW EXECUTE FUNCTION core.trg_fill_tenant_department_code_map();
CREATE TRIGGER trg_department_code_map_touch BEFORE UPDATE ON core.department_code_map
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();

ALTER TABLE core.department_code_map ENABLE ROW LEVEL SECURITY;
CREATE POLICY department_code_map_isolation ON core.department_code_map
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);

-- Seed: platform department templates (spec §3).
INSERT INTO core.template (kind, code, name, description, scope, usage) VALUES
    ('department', 'klinik_pratama',  'Klinik Pratama',            'Poli pelayanan medik dasar.', 'platform', 'copy'),
    ('department', 'klinik_utama',    'Klinik Utama',              'Poli dasar + spesialis yang umum di klinik utama.', 'platform', 'copy'),
    ('department', 'rumah_sakit',     'Rumah Sakit',               'Poli spesialis rumah sakit.', 'platform', 'copy'),
    ('department', 'rs_subspesialis', 'Rumah Sakit — Subspesialis', 'Poli subspesialis, diterapkan setelah template Rumah Sakit.', 'platform', 'copy'),
    ('department', 'layanan_program', 'Layanan Program',           'Layanan program (imunisasi, TB-DOTS, VCT, dll.).', 'platform', 'copy'),
    ('department', 'unit_penunjang',  'Unit Penunjang',            'Unit penunjang & IGD yang didaftarkan seperti poli.', 'platform', 'copy');

INSERT INTO core.template_department (template_id, code, name, specialty_code, sort_order)
SELECT t.id, d.code, d.name, d.specialty, d.sort_order
FROM core.template t
JOIN (VALUES
    ('klinik_pratama', 'UMU', 'Poli Umum', NULL, 1),
    ('klinik_pratama', 'GIG', 'Poli Gigi', NULL, 2),
    ('klinik_pratama', 'KIA', 'Poli KIA/KB', NULL, 3),
    ('klinik_pratama', 'GIZ', 'Poli Gizi', NULL, 4),
    ('klinik_pratama', 'LNS', 'Poli Lansia', NULL, 5),
    ('klinik_pratama', 'TND', 'Ruang Tindakan', NULL, 6),

    ('klinik_utama', 'UMU', 'Poli Umum', NULL, 1),
    ('klinik_utama', 'GIG', 'Poli Gigi', NULL, 2),
    ('klinik_utama', 'KIA', 'Poli KIA/KB', NULL, 3),
    ('klinik_utama', 'INT', 'Poli Penyakit Dalam', 'SPEC-DALAM', 4),
    ('klinik_utama', 'ANA', 'Poli Anak', 'SPEC-ANAK', 5),
    ('klinik_utama', 'OBG', 'Poli Obstetri & Ginekologi', 'SPEC-OBGYN', 6),
    ('klinik_utama', 'BED', 'Poli Bedah Umum', 'SPEC-BEDAH', 7),
    ('klinik_utama', 'MAT', 'Poli Mata', NULL, 8),
    ('klinik_utama', 'THT', 'Poli THT-KL', NULL, 9),
    ('klinik_utama', 'KLT', 'Poli Kulit & Kelamin', NULL, 10),
    ('klinik_utama', 'SAR', 'Poli Saraf', NULL, 11),
    ('klinik_utama', 'JIW', 'Poli Jiwa', NULL, 12),
    ('klinik_utama', 'PAR', 'Poli Paru', NULL, 13),
    ('klinik_utama', 'IRM', 'Poli Rehabilitasi Medik', NULL, 14),

    ('rumah_sakit', 'UMU', 'Poli Umum', NULL, 1),
    ('rumah_sakit', 'GIG', 'Poli Gigi', NULL, 2),
    ('rumah_sakit', 'INT', 'Poli Penyakit Dalam', 'SPEC-DALAM', 3),
    ('rumah_sakit', 'ANA', 'Poli Anak', 'SPEC-ANAK', 4),
    ('rumah_sakit', 'OBG', 'Poli Obstetri & Ginekologi', 'SPEC-OBGYN', 5),
    ('rumah_sakit', 'BED', 'Poli Bedah Umum', 'SPEC-BEDAH', 6),
    ('rumah_sakit', 'MAT', 'Poli Mata', NULL, 7),
    ('rumah_sakit', 'THT', 'Poli THT-KL', NULL, 8),
    ('rumah_sakit', 'KLT', 'Poli Kulit & Kelamin', NULL, 9),
    ('rumah_sakit', 'SAR', 'Poli Saraf', NULL, 10),
    ('rumah_sakit', 'JIW', 'Poli Jiwa', NULL, 11),
    ('rumah_sakit', 'PAR', 'Poli Paru', NULL, 12),
    ('rumah_sakit', 'IRM', 'Poli Rehabilitasi Medik', NULL, 13),
    ('rumah_sakit', 'JAN', 'Poli Jantung & Pembuluh Darah', NULL, 14),
    ('rumah_sakit', 'ORT', 'Poli Orthopedi & Traumatologi', NULL, 15),
    ('rumah_sakit', 'URO', 'Poli Urologi', NULL, 16),
    ('rumah_sakit', 'BSY', 'Poli Bedah Saraf', NULL, 17),
    ('rumah_sakit', 'BDA', 'Poli Bedah Anak', NULL, 18),
    ('rumah_sakit', 'BTK', 'Poli Bedah Toraks Kardiovaskular (BTKV)', NULL, 19),
    ('rumah_sakit', 'BPS', 'Poli Bedah Plastik Rekonstruksi & Estetik', NULL, 20),
    ('rumah_sakit', 'BDG', 'Poli Bedah Digestif', NULL, 21),
    ('rumah_sakit', 'BON', 'Poli Bedah Onkologi', NULL, 22),
    ('rumah_sakit', 'BEV', 'Poli Bedah Vaskular', NULL, 23),
    ('rumah_sakit', 'BEM', 'Poli Bedah Mulut', NULL, 24),
    ('rumah_sakit', 'GIZ', 'Poli Gizi Klinik', NULL, 25),
    ('rumah_sakit', 'GER', 'Poli Geriatri', NULL, 26),
    ('rumah_sakit', 'ANT', 'Poli Anestesi', NULL, 27),
    ('rumah_sakit', 'AND', 'Poli Andrologi', NULL, 28),
    ('rumah_sakit', 'KDO', 'Poli Kedokteran Okupasi', NULL, 29),
    ('rumah_sakit', 'KOR', 'Poli Kedokteran Olahraga', NULL, 30),
    ('rumah_sakit', 'AKP', 'Poli Akupunktur', NULL, 31),
    ('rumah_sakit', 'PSI', 'Poli Psikologi', NULL, 32),
    ('rumah_sakit', 'KIA', 'Poli KIA/KB', NULL, 33),

    ('rs_subspesialis', 'AAI', 'Poli Anak — Alergi Imunologi', NULL, 1),
    ('rs_subspesialis', 'AEN', 'Poli Anak — Endokrinologi', NULL, 2),
    ('rs_subspesialis', 'AGH', 'Poli Anak — Gastro-Hepatologi', NULL, 3),
    ('rs_subspesialis', 'AHO', 'Poli Anak — Hematologi Onkologi', NULL, 4),
    ('rs_subspesialis', 'AIPT', 'Poli Anak — Infeksi & Pediatri Tropis', NULL, 5),
    ('rs_subspesialis', 'AKA', 'Poli Anak — Kardiologi', NULL, 6),
    ('rs_subspesialis', 'ANE', 'Poli Anak — Nefrologi', NULL, 7),
    ('rs_subspesialis', 'ANEU', 'Poli Anak — Neurologi', NULL, 8),
    ('rs_subspesialis', 'ANPM', 'Poli Anak — Nutrisi & Penyakit Metabolik', NULL, 9),
    ('rs_subspesialis', 'PRAT', 'Poli Anak — Perinatologi', NULL, 10),
    ('rs_subspesialis', 'RSIA', 'Poli Anak — Respirologi', NULL, 11),
    ('rs_subspesialis', 'EMD', 'Poli Penyakit Dalam — Endokrin-Metabolik-Diabetes', NULL, 12),
    ('rs_subspesialis', 'GHE', 'Poli Penyakit Dalam — Gastroenterologi-Hepatologi', NULL, 13),
    ('rs_subspesialis', 'GHI', 'Poli Penyakit Dalam — Ginjal-Hipertensi', NULL, 14),
    ('rs_subspesialis', 'HOM', 'Poli Penyakit Dalam — Hematologi-Onkologi Medik', NULL, 15),
    ('rs_subspesialis', 'RMT', 'Poli Penyakit Dalam — Reumatologi', NULL, 16),
    ('rs_subspesialis', 'PTI', 'Poli Penyakit Dalam — Tropik-Infeksi', NULL, 17),
    ('rs_subspesialis', 'AIK', 'Poli Penyakit Dalam — Alergi-Imunologi Klinik', NULL, 18),
    ('rs_subspesialis', 'KVK', 'Poli Penyakit Dalam — Kardiovaskular', NULL, 19),
    ('rs_subspesialis', 'ELP', 'Poli Saraf — Epilepsi', NULL, 20),
    ('rs_subspesialis', 'NIM', 'Poli Saraf — Neuroinfeksi & Imunologi', NULL, 21),
    ('rs_subspesialis', 'NKL', 'Poli Saraf — Neurofisiologi Klinis', NULL, 22),
    ('rs_subspesialis', 'NMNN', 'Poli Saraf — Neurobehaviour, Neurogeriatri & Neurorestorasi', NULL, 23),
    ('rs_subspesialis', 'NNR', 'Poli Saraf — Neuropediatri & Neurokomunitas', NULL, 24),
    ('rs_subspesialis', 'NON', 'Poli Saraf — Neuro-oftalmologi & Neuro-otologi', NULL, 25),
    ('rs_subspesialis', 'NRTA', 'Poli Saraf — Neurotrauma', NULL, 26),
    ('rs_subspesialis', 'NSP', 'Poli Saraf — Neuromuskular & Saraf Perifer', NULL, 27),
    ('rs_subspesialis', 'SNNI', 'Poli Saraf — Serebrovaskular, Neurosonologi & Neurointervensi', NULL, 28),
    ('rs_subspesialis', 'NIT', 'Poli Saraf — Neuro-intensif', NULL, 29),
    ('rs_subspesialis', 'LFR', 'Poli THT-KL — Laringo-faringologi', NULL, 30),
    ('rs_subspesialis', 'OTL', 'Poli THT-KL — Otologi', NULL, 31),
    ('rs_subspesialis', 'RNLG', 'Poli THT-KL — Rinologi', NULL, 32),
    ('rs_subspesialis', 'BKSF', 'Poli THT-KL — Bronkoesofagologi', NULL, 33),
    ('rs_subspesialis', 'OKL', 'Poli THT-KL — Onkologi Kepala Leher', NULL, 34),
    ('rs_subspesialis', 'TKO', 'Poli THT-KL — THT Komunitas', NULL, 35),
    ('rs_subspesialis', 'FMT', 'Poli Obstetri & Ginekologi — Fetomaternal', NULL, 36),
    ('rs_subspesialis', 'OGI', 'Poli Obstetri & Ginekologi — Onkologi Ginekologi', NULL, 37),
    ('rs_subspesialis', 'OGSO', 'Poli Obstetri & Ginekologi — Obstetri Ginekologi Sosial', NULL, 38),
    ('rs_subspesialis', 'FTT', 'Poli Obstetri & Ginekologi — Fertilitas', NULL, 39),
    ('rs_subspesialis', 'URE', 'Poli Obstetri & Ginekologi — Uroginekologi Rekonstruksi', NULL, 40),
    ('rs_subspesialis', 'ASP', 'Poli Paru — Asma & PPOK', NULL, 41),
    ('rs_subspesialis', 'FPK', 'Poli Paru — Faal Paru Klinik', NULL, 42),
    ('rs_subspesialis', 'PKL', 'Poli Paru — Paru Kerja & Lingkungan', NULL, 43),
    ('rs_subspesialis', 'PIGD', 'Poli Paru — Pulmonologi Intervensi & Gawat Darurat Napas', NULL, 44),
    ('rs_subspesialis', 'OTR', 'Poli Paru — Onkologi Toraks', NULL, 45),
    ('rs_subspesialis', 'GOR', 'Poli Gigi — Ortodonti', NULL, 46),
    ('rs_subspesialis', 'KON', 'Poli Gigi — Pedodonti', NULL, 47),
    ('rs_subspesialis', 'GPR', 'Poli Gigi — Periodonti', NULL, 48),
    ('rs_subspesialis', 'PTD', 'Poli Gigi — Prostodonti', NULL, 49),
    ('rs_subspesialis', 'GND', 'Poli Gigi — Konservasi Gigi', NULL, 50),
    ('rs_subspesialis', 'PNM', 'Poli Gigi — Penyakit Mulut', NULL, 51),
    ('rs_subspesialis', 'GRD', 'Poli Gigi — Radiologi Gigi', NULL, 52),
    ('rs_subspesialis', 'AKS', 'Poli Anestesi — Kardiovaskular', NULL, 53),
    ('rs_subspesialis', 'ANO', 'Poli Anestesi — Obstetri', NULL, 54),
    ('rs_subspesialis', 'ANPE', 'Poli Anestesi — Pediatri', NULL, 55),
    ('rs_subspesialis', 'ARDI', 'Poli Anestesi — Regional & Intervensi', NULL, 56),
    ('rs_subspesialis', 'NRN', 'Poli Anestesi — Neuroanestesi', NULL, 57),
    ('rs_subspesialis', 'MN', 'Poli Anestesi — Manajemen Nyeri', NULL, 58),
    ('rs_subspesialis', 'HBT', 'Poli Bedah — Bedah Tangan', NULL, 59),
    ('rs_subspesialis', 'BLB', 'Poli Bedah — Luka Bakar', NULL, 60),
    ('rs_subspesialis', 'MSU', 'Poli Bedah — Bedah Mikro', NULL, 61),
    ('rs_subspesialis', 'KIL', 'Poli Bedah — Kraniofasial', NULL, 62),

    ('layanan_program', 'IMS', 'Imunisasi / Klinik Vaksin', NULL, 1),
    ('layanan_program', 'TBC', 'TB-DOTS', NULL, 2),
    ('layanan_program', 'VCT', 'VCT / HIV', NULL, 3),
    ('layanan_program', 'LKT', 'Klinik Laktasi', NULL, 4),
    ('layanan_program', 'KRE', 'Kesehatan Remaja', NULL, 5),
    ('layanan_program', 'DBM', 'Klinik Kaki & Edukasi Diabetes', NULL, 6),
    ('layanan_program', 'KTK', 'Tumbuh Kembang', NULL, 7),

    ('unit_penunjang', 'IGD', 'Instalasi Gawat Darurat', NULL, 1),
    ('unit_penunjang', 'LAB', 'Laboratorium Klinik', NULL, 2),
    ('unit_penunjang', 'PAT', 'Patologi Anatomi', NULL, 3),
    ('unit_penunjang', 'RAD', 'Radiologi', NULL, 4),
    ('unit_penunjang', 'CTS', 'CT Scan', NULL, 5),
    ('unit_penunjang', 'MRI', 'MRI', NULL, 6),
    ('unit_penunjang', 'USG', 'USG', NULL, 7),
    ('unit_penunjang', 'ECO', 'Ekokardiografi', NULL, 8),
    ('unit_penunjang', 'EKG', 'Rekam Jantung (EKG)', NULL, 9),
    ('unit_penunjang', 'TRD', 'Treadmill Test', NULL, 10),
    ('unit_penunjang', 'HDL', 'Hemodialisa', NULL, 11),
    ('unit_penunjang', 'CAP', 'CAPD', NULL, 12),
    ('unit_penunjang', 'FIS', 'Fisioterapi', NULL, 13),
    ('unit_penunjang', 'ESW', 'ESWL', NULL, 14),
    ('unit_penunjang', 'KDN', 'Kedokteran Nuklir', NULL, 15),
    ('unit_penunjang', 'RAT', 'Radioterapi', NULL, 16),
    ('unit_penunjang', 'MCU', 'Medical Check Up', NULL, 17),
    ('unit_penunjang', 'ODC', 'One Day Care', NULL, 18),
    ('unit_penunjang', 'APT', 'Farmasi Rawat Jalan', NULL, 19),
    ('unit_penunjang', 'OPT', 'Optik', NULL, 20),
    ('unit_penunjang', 'TEL', 'Telekonsultasi', NULL, 21),
    ('unit_penunjang', 'HOME', 'Kunjungan Rumah (Home Visit)', NULL, 22)
) AS d(template_code, code, name, specialty, sort_order) ON d.template_code = t.code
WHERE t.scope = 'platform' AND t.kind = 'department';

-- Default BPJS VClaim mapping = reference-data code (spec §4); PCare left empty
-- (codes unverified); Klinik Pratama internal-only codes (LNS, TND) get none.
INSERT INTO core.template_department_code_map (template_department_id, system, code)
SELECT td.id, 'bpjs-vclaim', td.code
FROM core.template_department td
JOIN core.template t ON t.id = td.template_id AND t.scope = 'platform' AND t.kind = 'department'
WHERE t.code <> 'klinik_pratama';

-- Seed guard: a missing row would silently shrink a template.
DO $$
DECLARE n integer;
BEGIN
    SELECT count(*) INTO n FROM core.template_department td
      JOIN core.template t ON t.id = td.template_id AND t.kind = 'department' AND t.scope = 'platform';
    IF n <> 6 + 14 + 33 + 62 + 7 + 22 THEN
        RAISE EXCEPTION 'department template seed count % <> 144', n;
    END IF;
END;
$$;
