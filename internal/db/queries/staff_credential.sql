-- internal/db/queries/staff_credential.sql
-- core.staff_credential (spec 2026-10-20-staff-credential-expiry-design §4.1-§4.2)

-- name: CreateStaffCredential :one
INSERT INTO core.staff_credential (company_id, merchant_id, user_id, credential_kind, license_number, valid_until, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7)
RETURNING *;

-- name: GetStaffCredentialByID :one
SELECT * FROM core.staff_credential WHERE id = $1 AND deleted_at IS NULL;

-- name: GetUserForCredential :one
SELECT u.id, u.email, u.company_id FROM core.app_user u
WHERE u.id = $1 AND u.company_id = $2 AND u.deleted_at IS NULL
  AND EXISTS (SELECT 1 FROM core.user_merchant_role umr
              WHERE umr.user_id = u.id AND umr.merchant_id = $3);

-- name: UpdateStaffCredential :one
UPDATE core.staff_credential
SET license_number = $2, valid_until = $3
WHERE id = $1 AND deleted_at IS NULL
RETURNING *;

-- name: SoftDeleteStaffCredential :exec
UPDATE core.staff_credential
SET deleted_at = now(), deleted_by = $2
WHERE id = $1 AND deleted_at IS NULL;

-- name: ListStaffCredentialStatus :many
SELECT p.merchant_id, p.company_id, 'physician'::text AS staff_kind,
       au.id AS user_id, COALESCE(pe.full_name, au.username, '') AS staff_name,
       'str'::text AS credential_kind, p.str_number AS license_number, NULL::date AS valid_until
FROM core.physician p
JOIN core.person pe ON pe.id = p.person_id
LEFT JOIN core.app_user au ON au.id = (
    SELECT au2.id FROM core.app_user au2
    WHERE au2.person_id = p.person_id AND au2.deleted_at IS NULL AND au2.is_active
    ORDER BY au2.created_at
    LIMIT 1
)
WHERE p.merchant_id = $1 AND p.deleted_at IS NULL AND p.str_number IS NOT NULL
UNION ALL
SELECT p.merchant_id, p.company_id, 'physician'::text AS staff_kind,
       au.id AS user_id, COALESCE(pe.full_name, au.username, '') AS staff_name,
       'sip'::text AS credential_kind, p.sip_number AS license_number, p.sip_valid_until AS valid_until
FROM core.physician p
JOIN core.person pe ON pe.id = p.person_id
LEFT JOIN core.app_user au ON au.id = (
    SELECT au2.id FROM core.app_user au2
    WHERE au2.person_id = p.person_id AND au2.deleted_at IS NULL AND au2.is_active
    ORDER BY au2.created_at
    LIMIT 1
)
WHERE p.merchant_id = $1 AND p.deleted_at IS NULL AND p.sip_number IS NOT NULL
UNION ALL
SELECT sc.merchant_id, sc.company_id, 'staff'::text AS staff_kind,
       u.id AS user_id, COALESCE(pe2.full_name, u.username, '') AS staff_name,
       sc.credential_kind, sc.license_number, sc.valid_until
FROM core.staff_credential sc
JOIN core.app_user u ON u.id = sc.user_id AND u.deleted_at IS NULL
LEFT JOIN core.person pe2 ON pe2.id = u.person_id
WHERE sc.merchant_id = $1 AND sc.deleted_at IS NULL;
