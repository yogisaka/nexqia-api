-- internal/db/queries/terminology.sql
-- terminology.concept search by code_system name. Long datasets (wilayah,
-- option, diagnosis) are consumed by the pendaftaran form comboboxes; the
-- code_system name is the stable, human-facing selector (e.g. "Kemendagri
-- Region", "icd-10"). trigram index on display exists (000005).

-- name: SearchConceptsBySystem :many
SELECT c.id, c.code, c.display
FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE (cs.name ILIKE $1 OR cs.system_uri ILIKE '%' || $1 || '%')
  AND (c.code ILIKE '%' || $2 || '%' OR c.display ILIKE '%' || $2 || '%')
  AND c.is_selectable AND c.is_active
ORDER BY c.code
LIMIT $3;

-- name: ListConceptsBySystem :many
-- Unpaginated dump of one code_system, for client-side bulk preload/caching
-- (e.g. the full ~87k-row Kemendagri Region tree into IndexedDB). 100000 cap
-- is a hardcoded safety ceiling, not a tunable — no code_system this big yet.
SELECT c.id, c.code, c.display
FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE (cs.name ILIKE $1 OR cs.system_uri ILIKE '%' || $1 || '%')
  AND c.is_selectable AND c.is_active
ORDER BY c.code
LIMIT 100000;

-- name: GetActiveConceptBySystemName :one
SELECT c.id, c.code, c.display
FROM terminology.concept c
JOIN terminology.code_system cs ON cs.id = c.code_system_id
WHERE cs.name = sqlc.arg('system') AND cs.is_active AND c.code = sqlc.arg('code') AND c.is_active;

-- name: ListMapCandidates :many
-- Conversion candidates source → target system across every active map set
-- (spec §6.2): preferred first, then by target code.
SELECT cm.map_set, cm.target_code, cm.relationship, cm.is_preferred,
       t.code AS stem_code, t.display, t.is_selectable,
       COALESCE(t.properties->>'linearization_uri', '')::text AS uri
FROM terminology.concept_map cm
JOIN terminology.concept t ON t.id = cm.target_concept_id AND t.is_active
JOIN terminology.code_system ts ON ts.id = t.code_system_id
WHERE cm.source_concept_id = sqlc.arg('source_id') AND cm.is_active AND ts.name = sqlc.arg('target')
ORDER BY cm.is_preferred DESC, cm.target_code;
