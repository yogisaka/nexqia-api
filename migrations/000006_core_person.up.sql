-- migrations/000006_core_person.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §4 "Person master".

CREATE TABLE core.person (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid NOT NULL REFERENCES core.company(id),  -- LEVEL COMPANY, bukan merchant
    nik             text,                     -- encrypted via pgcrypto di app layer, lihat catatan bawah
    medical_record_no text,                   -- nomor RM, unik per company
    full_name       text NOT NULL,
    birth_date      date,
    birth_place     text,
    gender          text NOT NULL CHECK (gender IN ('male','female')),
    blood_type      text,
    marital_status  text,
    religion_concept_id uuid REFERENCES terminology.concept(id),
    education_concept_id uuid REFERENCES terminology.concept(id),
    ethnicity_concept_id uuid REFERENCES terminology.concept(id),
    phone           text,
    email           text,
    address         text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    created_by      uuid,
    updated_at      timestamptz NOT NULL DEFAULT now(),
    updated_by      uuid,
    deleted_at      timestamptz,
    deleted_by      uuid,
    row_version     integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_person_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE UNIQUE INDEX uq_person_nik ON core.person (company_id, nik) WHERE deleted_at IS NULL AND nik IS NOT NULL;
CREATE UNIQUE INDEX uq_person_mrn ON core.person (company_id, medical_record_no) WHERE deleted_at IS NULL AND medical_record_no IS NOT NULL;
CREATE INDEX idx_person_name_trgm ON core.person USING gin (full_name gin_trgm_ops);
CREATE TRIGGER trg_person_touch BEFORE UPDATE ON core.person
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.person ENABLE ROW LEVEL SECURITY;
CREATE POLICY person_isolation ON core.person
    USING (company_id = current_setting('app.current_company_id')::uuid);

ALTER TABLE core.person ADD COLUMN family_id uuid REFERENCES core.person(id);  -- self-ref ke kepala keluarga, nullable

-- alergi pasien (obat/makanan/lingkungan) — patient-safety data, ikut person (bukan per-admission), company-scoped
-- data lama (m_person_allergy) mostly teks bebas, link ke drug/ingredient master nyaris tidak pernah dipakai (<1%)
CREATE TABLE core.patient_allergy (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id     uuid NOT NULL REFERENCES core.company(id),
    person_id      uuid NOT NULL REFERENCES core.person(id),
    allergy_type   text NOT NULL CHECK (allergy_type IN ('drug','food','environment','insect','other')),
    substance_name text NOT NULL,
    reaction       text,
    severity       text,
    is_active      boolean NOT NULL DEFAULT true,
    recorded_at    timestamptz NOT NULL DEFAULT now(),
    recorded_by    uuid,
    updated_at     timestamptz NOT NULL DEFAULT now(),
    updated_by     uuid,
    row_version    integer NOT NULL DEFAULT 1
);
CREATE INDEX idx_patient_allergy_person ON core.patient_allergy (person_id) WHERE is_active;
CREATE TRIGGER trg_patient_allergy_touch BEFORE UPDATE ON core.patient_allergy
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.patient_allergy ENABLE ROW LEVEL SECURITY;
CREATE POLICY patient_allergy_isolation ON core.patient_allergy
    USING (company_id = current_setting('app.current_company_id')::uuid);

-- history merge nomor RM — audit trail wajib, bukan destructive merge
CREATE TABLE core.person_merge_log (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    surviving_person_id uuid NOT NULL REFERENCES core.person(id),
    merged_person_id    uuid NOT NULL REFERENCES core.person(id),
    reason              text,
    merged_at           timestamptz NOT NULL DEFAULT now(),
    merged_by           uuid NOT NULL
);

ALTER TABLE core.app_user
    ADD CONSTRAINT fk_user_person FOREIGN KEY (person_id) REFERENCES core.person(id);

-- Catatan enkripsi: nik wajib kolom terenkripsi (pgcrypto pgp_sym_encrypt) di app layer, tidak boleh plaintext.
