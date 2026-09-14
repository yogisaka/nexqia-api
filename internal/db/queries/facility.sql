-- internal/db/queries/facility.sql
-- core.department, core.ward, core.bed (docs/07-core-ddl.md §6 "Master Data fasilitas")

-- name: CreateDepartment :one
INSERT INTO core.department (company_id, merchant_id, code, name, specialty_concept_id, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $6)
RETURNING *;

-- name: GetDepartmentByID :one
SELECT * FROM core.department WHERE id = $1 AND deleted_at IS NULL;

-- name: ListDepartmentsByMerchant :many
SELECT * FROM core.department
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: UpdateDepartment :one
UPDATE core.department
SET name = $2, specialty_concept_id = $3, is_active = $4, updated_by = $5
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteDepartment :exec
UPDATE core.department
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateWard :one
INSERT INTO core.ward (company_id, merchant_id, department_id, code, name, ward_class, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $7)
RETURNING *;

-- name: GetWardByID :one
SELECT * FROM core.ward WHERE id = $1 AND deleted_at IS NULL;

-- name: ListWardsByMerchant :many
SELECT * FROM core.ward
WHERE merchant_id = $1 AND deleted_at IS NULL
ORDER BY name
LIMIT $2 OFFSET $3;

-- name: ListWardsByDepartment :many
SELECT * FROM core.ward WHERE department_id = $1 AND deleted_at IS NULL ORDER BY name;

-- name: UpdateWard :one
UPDATE core.ward
SET name = $2, department_id = $3, ward_class = $4, is_active = $5, updated_by = $6
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteWard :exec
UPDATE core.ward
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: CreateBed :one
INSERT INTO core.bed (company_id, merchant_id, ward_id, code, created_by, updated_by)
VALUES ($1, $2, $3, $4, $5, $5)
RETURNING *;

-- name: GetBedByID :one
SELECT * FROM core.bed WHERE id = $1 AND deleted_at IS NULL;

-- name: ListBedsByWard :many
SELECT * FROM core.bed WHERE ward_id = $1 AND deleted_at IS NULL ORDER BY code;

-- name: ListBedsByStatus :many
SELECT * FROM core.bed WHERE merchant_id = $1 AND status = $2 AND deleted_at IS NULL ORDER BY code;

-- name: UpdateBedStatus :one
UPDATE core.bed
SET status = $2, updated_by = $3
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteBed :exec
UPDATE core.bed
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;
