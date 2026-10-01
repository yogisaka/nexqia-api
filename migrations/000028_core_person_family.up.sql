-- migrations/000028_core_person_family.up.sql
-- Verbatim transcript of
-- 2026-09-18-fo-pendaftaran-full-design.md §4.1 + §4.2.
-- Person concept-FK columns + core.person_family (family member master).

-- §4.1 core.person — kolom baru (semua concept FK)
ALTER TABLE core.person ADD COLUMN region_village_concept_id uuid REFERENCES terminology.concept(id);
ALTER TABLE core.person ADD COLUMN citizenship_concept_id uuid REFERENCES terminology.concept(id);
ALTER TABLE core.person ADD COLUMN identity_type_concept_id uuid REFERENCES terminology.concept(id);
ALTER TABLE core.person ADD COLUMN job_concept_id uuid REFERENCES terminology.concept(id);
ALTER TABLE core.person ADD COLUMN marital_status_concept_id uuid REFERENCES terminology.concept(id);
ALTER TABLE core.person ADD COLUMN blood_type_concept_id uuid REFERENCES terminology.concept(id);

-- §4.2 core.person_family (baru)
CREATE TABLE core.person_family (
    id            uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id    uuid NOT NULL REFERENCES core.company(id),
    person_id     uuid NOT NULL REFERENCES core.person(id),
    full_name     text NOT NULL,
    phone         text,
    email         text,
    birth_date    date,
    birth_place   text,
    gender        text CHECK (gender IN ('male','female')),
    address       text,
    region_village_concept_id uuid REFERENCES terminology.concept(id),
    postal_code   text,
    relationship_concept_id uuid REFERENCES terminology.concept(id), -- family_relationship_status
    position_concept_id uuid REFERENCES terminology.concept(id),      -- person_relationship
    job_concept_id uuid REFERENCES terminology.concept(id),
    is_responsible_person boolean NOT NULL DEFAULT false,
    created_at    timestamptz NOT NULL DEFAULT now(),
    created_by    uuid,
    updated_at    timestamptz NOT NULL DEFAULT now(),
    updated_by    uuid,
    deleted_at    timestamptz,
    deleted_by    uuid,
    row_version   integer NOT NULL DEFAULT 1,
    CONSTRAINT chk_person_family_soft_delete CHECK (
        (deleted_at IS NULL AND deleted_by IS NULL) OR
        (deleted_at IS NOT NULL AND deleted_by IS NOT NULL)
    )
);
CREATE INDEX idx_person_family_person ON core.person_family (person_id) WHERE deleted_at IS NULL;
CREATE TRIGGER trg_person_family_touch BEFORE UPDATE ON core.person_family
    FOR EACH ROW EXECUTE FUNCTION trg_touch_row();
ALTER TABLE core.person_family ENABLE ROW LEVEL SECURITY;
CREATE POLICY person_family_isolation ON core.person_family
    USING (company_id = current_setting('app.current_company_id')::uuid);
