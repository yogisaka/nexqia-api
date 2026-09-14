-- migrations/000005_core_terminology.up.sql
-- Verbatim transcript of docs/07-core-ddl.md §3 "Terminology Service".

CREATE TABLE terminology.code_system (
    id           uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    system_uri   text NOT NULL UNIQUE,   -- "http://hl7.org/fhir/sid/icd-10", dst
    name         text NOT NULL,          -- "ICD-10", "ICD-9-CM", "ICD-11", "LOINC", "KFA"
    version      text NOT NULL,
    is_active    boolean NOT NULL DEFAULT true,
    created_at   timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE terminology.concept (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    code_system_id uuid NOT NULL REFERENCES terminology.code_system(id),
    code           text NOT NULL,
    display        text NOT NULL,
    definition     text,
    is_active      boolean NOT NULL DEFAULT true,
    created_at     timestamptz NOT NULL DEFAULT now(),
    UNIQUE (code_system_id, code)
);
CREATE INDEX idx_concept_display_trgm ON terminology.concept USING gin (display gin_trgm_ops);

-- edge table, dukung multi-parent (wajib buat ICD-11 foundation model)
CREATE TABLE terminology.concept_hierarchy (
    parent_concept_id  uuid NOT NULL REFERENCES terminology.concept(id),
    child_concept_id   uuid NOT NULL REFERENCES terminology.concept(id),
    relationship_type  text NOT NULL DEFAULT 'is-a',
    PRIMARY KEY (parent_concept_id, child_concept_id)
);
