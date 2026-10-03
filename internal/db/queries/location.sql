-- internal/db/queries/location.sql
-- core.location physical tree (spec 2026-10-01-a-physical-location-design §3-§4).
-- latitude/longitude are numeric(9,6): float params are cast ::float8 (the DB
-- casts to numeric); reads return the raw numeric and the handler converts.

-- name: CreateLocation :one
INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions, service_class, capacity, latitude, longitude, status, sort_order, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8, $9, $10, $11, $12, $13, $14, $14)
RETURNING *;

-- name: GetLocationByID :one
SELECT * FROM core.location WHERE id = $1 AND deleted_at IS NULL;

-- name: ListLocationsByMerchant :many
SELECT * FROM core.location
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY sort_order, code
LIMIT $2 OFFSET $3;

-- name: ListLocationsByMerchantKind :many
SELECT * FROM core.location
WHERE merchant_id = $1 AND deleted_at IS NULL AND kind = $2
ORDER BY sort_order, code
LIMIT $3 OFFSET $4;

-- name: ListLocationsByMerchantFunction :many
SELECT * FROM core.location
WHERE merchant_id = $1 AND deleted_at IS NULL AND $2::text = ANY(functions)
ORDER BY sort_order, code
LIMIT $3 OFFSET $4;

-- name: ListAllLocationsByMerchant :many
SELECT * FROM core.location
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY sort_order, code;

-- name: UpdateLocation :one
UPDATE core.location
SET parent_id = $2, code = $3, name = $4, functions = $5, service_class = $6,
    capacity = $7, latitude = $8, longitude = $9,
    status = $10, sort_order = $11, updated_by = $12
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: CountActiveChildrenByParent :one
SELECT count(*) FROM core.location WHERE parent_id = $1 AND deleted_at IS NULL;

-- name: CountActiveBedsByParent :one
SELECT count(*) FROM core.location WHERE parent_id = $1 AND kind = 'bed' AND deleted_at IS NULL;

-- name: SoftDeleteLocation :exec
UPDATE core.location
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
