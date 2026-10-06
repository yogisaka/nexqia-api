-- nexqia-api/migrations/000062_terminology_import.up.sql
-- Spec 2026-10-01-terminology-import-design §3 (+ §12 review 2026-10-06):
-- release metadata + standard profile on code_system, selectability and
-- per-standard properties on concept, cross-standard concept_map, and
-- terminology made read-only for app_runtime (loaded by migration/CLI only).

ALTER TABLE terminology.code_system
    ADD COLUMN source_url    text,
    ADD COLUMN source_sha256 text,
    ADD COLUMN license       text,
    ADD COLUMN attribution   text,
    ADD COLUMN imported_at   timestamptz,
    ADD COLUMN profile       jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT chk_code_system_profile_object CHECK (jsonb_typeof(profile) = 'object');

ALTER TABLE terminology.concept
    ADD COLUMN is_selectable boolean NOT NULL DEFAULT true,
    ADD COLUMN properties    jsonb NOT NULL DEFAULT '{}'::jsonb
        CONSTRAINT chk_concept_properties_object CHECK (jsonb_typeof(properties) = 'object');

CREATE INDEX idx_concept_system_selectable ON terminology.concept (code_system_id)
    WHERE is_selectable AND is_active;

CREATE TABLE terminology.concept_map (
    id                uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    map_set           text NOT NULL,
    source_concept_id uuid NOT NULL REFERENCES terminology.concept(id),
    target_concept_id uuid NOT NULL REFERENCES terminology.concept(id),
    -- exact target code from the release: the target concept's own code, or
    -- an ICD-11 cluster (stem&ext, stem/stem) whose stem is target_concept_id
    target_code       text NOT NULL,
    relationship      text NOT NULL DEFAULT 'related-to'
        CONSTRAINT chk_concept_map_relationship CHECK (relationship IN
            ('equivalent', 'source-is-narrower-than-target', 'source-is-broader-than-target', 'related-to')),
    is_preferred      boolean NOT NULL DEFAULT false,
    is_active         boolean NOT NULL DEFAULT true,
    created_at        timestamptz NOT NULL DEFAULT now(),
    CONSTRAINT uq_concept_map UNIQUE (map_set, source_concept_id, target_code)
);
CREATE INDEX idx_concept_map_source ON terminology.concept_map (source_concept_id) WHERE is_active;

-- AFTER the CREATE: default privileges on schema terminology (000002) gave
-- app_runtime write access to every new table, concept_map included.
REVOKE INSERT, UPDATE, DELETE ON terminology.code_system, terminology.concept,
    terminology.concept_hierarchy, terminology.concept_map FROM app_runtime;
GRANT SELECT ON terminology.concept_map TO app_runtime;
