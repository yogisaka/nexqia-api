-- internal/db/queries/terminology.sql
-- terminology.code_system, terminology.concept, terminology.concept_hierarchy
-- (docs/07-core-ddl.md §3 "Terminology Service")

-- name: CreateCodeSystem :one
INSERT INTO terminology.code_system (system_uri, name, version)
VALUES ($1, $2, $3)
RETURNING *;

-- name: GetCodeSystemByURI :one
SELECT * FROM terminology.code_system WHERE system_uri = $1;

-- name: ListCodeSystems :many
SELECT * FROM terminology.code_system WHERE is_active ORDER BY name;

-- name: UpdateCodeSystem :one
UPDATE terminology.code_system
SET name = $2, version = $3, is_active = $4
WHERE id = $1
RETURNING *;

-- name: CreateConcept :one
INSERT INTO terminology.concept (code_system_id, code, display, definition)
VALUES ($1, $2, $3, $4)
RETURNING *;

-- name: GetConceptByID :one
SELECT * FROM terminology.concept WHERE id = $1;

-- name: GetConceptByCode :one
SELECT * FROM terminology.concept WHERE code_system_id = $1 AND code = $2;

-- name: ListConceptsByCodeSystem :many
SELECT * FROM terminology.concept
WHERE code_system_id = $1 AND is_active
ORDER BY display
LIMIT $2 OFFSET $3;

-- name: SearchConceptsByDisplay :many
SELECT * FROM terminology.concept
WHERE code_system_id = $1 AND is_active AND display ILIKE '%' || $2 || '%'
ORDER BY display
LIMIT $3;

-- name: UpdateConcept :one
UPDATE terminology.concept
SET display = $2, definition = $3, is_active = $4
WHERE id = $1
RETURNING *;

-- name: AddConceptHierarchy :exec
INSERT INTO terminology.concept_hierarchy (parent_concept_id, child_concept_id, relationship_type)
VALUES ($1, $2, $3)
ON CONFLICT (parent_concept_id, child_concept_id) DO NOTHING;

-- name: RemoveConceptHierarchy :exec
DELETE FROM terminology.concept_hierarchy WHERE parent_concept_id = $1 AND child_concept_id = $2;

-- name: ListChildConcepts :many
SELECT c.* FROM terminology.concept c
JOIN terminology.concept_hierarchy h ON h.child_concept_id = c.id
WHERE h.parent_concept_id = $1;

-- name: ListParentConcepts :many
SELECT c.* FROM terminology.concept c
JOIN terminology.concept_hierarchy h ON h.parent_concept_id = c.id
WHERE h.child_concept_id = $1;
