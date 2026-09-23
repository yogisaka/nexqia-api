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
ORDER BY c.code
LIMIT 100000;
