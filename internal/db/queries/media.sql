-- internal/db/queries/media.sql
-- core.media, core.media_link (docs/07-core-ddl.md §11 "Media Management")

-- name: CreateMedia :one
INSERT INTO core.media (
    company_id, merchant_id, storage_provider, storage_key, original_filename, mime_type,
    size_bytes, checksum_sha256, uploaded_by
) VALUES (
    $1, $2, $3, $4, $5, $6, $7, $8, $9
)
RETURNING *;

-- name: GetMediaByID :one
SELECT * FROM core.media WHERE id = $1 AND deleted_at IS NULL;

-- name: GetMediaByChecksum :one
SELECT * FROM core.media WHERE merchant_id = $1 AND checksum_sha256 = $2 AND deleted_at IS NULL;

-- name: ListMediaByMerchant :many
SELECT * FROM core.media
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY created_at DESC
LIMIT $2 OFFSET $3;

-- name: UpdateMediaStatus :one
UPDATE core.media
SET status = $2, virus_scan_status = $3
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteMedia :exec
UPDATE core.media
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateMediaLink :one
INSERT INTO core.media_link (media_id, owner_table, owner_id, purpose, created_by)
VALUES ($1, $2, $3, $4, $5)
ON CONFLICT (media_id, owner_table, owner_id, purpose) DO NOTHING
RETURNING *;

-- name: ListMediaLinksByOwner :many
SELECT ml.*, m.storage_key, m.mime_type, m.original_filename
FROM core.media_link ml
JOIN core.media m ON m.id = ml.media_id
WHERE ml.owner_table = $1 AND ml.owner_id = $2 AND ml.deleted_at IS NULL AND m.deleted_at IS NULL;

-- name: ListMediaLinksByMedia :many
SELECT * FROM core.media_link WHERE media_id = $1 AND deleted_at IS NULL;

-- name: SoftDeleteMediaLink :exec
UPDATE core.media_link
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
