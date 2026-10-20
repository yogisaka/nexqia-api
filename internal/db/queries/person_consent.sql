-- internal/db/queries/person_consent.sql
-- Patient data-processing consent (spec 2026-10-17-patient-documents-design
-- §5.3): append-only core.person_consent rows plus the active consent_text
-- version feed (migration 000074).

-- name: CreatePersonConsent :one
INSERT INTO core.person_consent
    (company_id, merchant_id, person_id, consent_type, text_version, method, person_document_id, granted_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: ListPersonConsents :many
SELECT pc.*, per.full_name AS granted_by_name
FROM core.person_consent pc
JOIN core.app_user au ON au.id = pc.granted_by
JOIN core.person per ON per.id = au.person_id
WHERE pc.person_id = $1
ORDER BY pc.granted_at DESC, pc.id DESC;

-- name: GetLatestPersonConsent :one
SELECT pc.*, per.full_name AS granted_by_name
FROM core.person_consent pc
JOIN core.app_user au ON au.id = pc.granted_by
JOIN core.person per ON per.id = au.person_id
WHERE pc.person_id = $1
ORDER BY pc.granted_at DESC, pc.id DESC
LIMIT 1;

-- name: GetActiveConsentText :one
SELECT code, version, body FROM core.consent_text
WHERE code = $1 AND is_active
ORDER BY created_at DESC, id DESC
LIMIT 1;
