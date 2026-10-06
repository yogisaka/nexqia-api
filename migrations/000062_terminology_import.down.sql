-- nexqia-api/migrations/000062_terminology_import.down.sql
GRANT INSERT, UPDATE, DELETE ON terminology.code_system, terminology.concept,
    terminology.concept_hierarchy TO app_runtime;
DROP TABLE IF EXISTS terminology.concept_map;
DROP INDEX IF EXISTS terminology.idx_concept_system_selectable;
ALTER TABLE terminology.concept
    DROP COLUMN IF EXISTS properties,
    DROP COLUMN IF EXISTS is_selectable;
ALTER TABLE terminology.code_system
    DROP COLUMN IF EXISTS profile,
    DROP COLUMN IF EXISTS imported_at,
    DROP COLUMN IF EXISTS attribution,
    DROP COLUMN IF EXISTS license,
    DROP COLUMN IF EXISTS source_sha256,
    DROP COLUMN IF EXISTS source_url;
