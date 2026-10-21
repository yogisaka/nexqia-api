-- internal/db/queries/connect.sql
-- Connect pillar (Permenkes 6/2026 Fase C1-C3, spec
-- 2026-10-20-permenkes-6-2026-compliance-design): all integration queries live
-- here. The worker reads satusehat_resource_map ONLY through the SECURITY
-- DEFINER claim function — RLS tenant GUC never sticks to pool connections.

-- The worker claims rows via pool.Query against connect.claim_satusehat_maps
-- (SECURITY DEFINER, RETURNS TABLE) — sqlc cannot type that result, so the
-- claim scan lives in internal/connect/worker.go.
-- UpdateLocationSatusehatID already lives in location.sql.

-- name: MarkSatusehatMapSynced :exec
UPDATE connect.satusehat_resource_map
SET sync_status = 'synced', fhir_resource_id = $2, last_synced_at = now(),
    last_error = NULL, locked_until = NULL
WHERE id = $1;

-- name: MarkSatusehatMapFailed :exec
UPDATE connect.satusehat_resource_map
SET sync_status = 'failed', last_error = $2, locked_until = NULL
WHERE id = $1;

-- name: GetMerchantForSatusehat :one
SELECT id, company_id, code, name, kemkes_facility_code
FROM core.merchant WHERE id = $1 AND deleted_at IS NULL;

-- name: GetSatusehatConfigByMerchant :one
SELECT base_url, config, is_active, (credential_encrypted IS NOT NULL) AS credential_set
FROM connect.integration_config
WHERE merchant_id = $1
  AND provider_id = (SELECT id FROM connect.integration_provider WHERE code = 'satusehat');

-- name: UpsertSatusehatConfig :exec
INSERT INTO connect.integration_config
    (company_id, merchant_id, provider_id, base_url, config, credential_encrypted, is_active, created_by, updated_by)
VALUES ($1, $2, (SELECT id FROM connect.integration_provider WHERE code = 'satusehat'),
        $3, $4, pgp_sym_encrypt($5::text, $6::text), $7, $8, $8)
ON CONFLICT (merchant_id, provider_id) DO UPDATE SET
    base_url = EXCLUDED.base_url,
    config = EXCLUDED.config,
    credential_encrypted = EXCLUDED.credential_encrypted,
    is_active = EXCLUDED.is_active,
    updated_by = EXCLUDED.updated_by;

-- name: ListSatusehatLocations :many
SELECT id, code, name, latitude, longitude
FROM core.location
WHERE merchant_id = $1 AND deleted_at IS NULL AND kind <> 'bed'
ORDER BY code;

-- name: UpsertSatusehatResourceMap :one
-- Jalur re-enqueue EKSPLISIT (tombol sync): SELALU pending + reset lock —
-- tanpa CASE (review 2026-10-20 temuan 1): baris 'failed'/'sending' basi
-- pun dijadwalkan ulang; worker hanya meng-claim 'pending'/'sending' basi
-- (connect.claim_satusehat_maps, plan C1).
INSERT INTO connect.satusehat_resource_map
    (company_id, merchant_id, local_table, local_id, fhir_resource_type, sync_status, last_payload)
VALUES ($1, $2, $3, $4, $5, 'pending', $6::jsonb)
ON CONFLICT (local_table, local_id, fhir_resource_type) DO UPDATE SET
    last_payload = EXCLUDED.last_payload,
    sync_status = 'pending',
    locked_until = NULL,
    last_error = NULL
RETURNING id;

-- name: ListSatusehatResourceStatusByMerchant :many
SELECT id, local_table, local_id, fhir_resource_type, fhir_resource_id,
       sync_status, last_error, last_synced_at, updated_at
FROM connect.satusehat_resource_map
WHERE merchant_id = $1
ORDER BY updated_at DESC
LIMIT 200;

-- name: GetSatusehatMapByLocal :one
-- Tidak ada baris ⇒ pgx.ErrNoRows; Pemanggil WAJIB memetakan ErrNoRows ⇒
-- nilai default (""/belum-synced), BUKAN error 500 (review temuan 3).
SELECT id, fhir_resource_id, sync_status
FROM connect.satusehat_resource_map
WHERE local_table = $1 AND local_id = $2 AND fhir_resource_type = $3;

-- name: GetPersonForSatusehat :one
-- Kolom nik = CIPHERTEXT pgcrypto base64 (000006:7,81) — HANYA input
-- decryptNIK di handler; DILARANG dipakai langsung di payload FHIR
-- (temuan review 1).
SELECT id, company_id, nik, full_name, gender, birth_date
FROM core.person WHERE id = $1 AND deleted_at IS NULL;

-- name: GetAdmissionForSatusehatEncounter :one
SELECT id, company_id, merchant_id, visit_no, person_id, admission_type,
       status, admission_at, discharge_at
FROM operations.admission WHERE id = $1 AND deleted_at IS NULL;

-- name: GetSatusehatResourceMapByID :one
SELECT id, merchant_id, local_table, local_id, fhir_resource_type,
       fhir_resource_id, sync_status
FROM connect.satusehat_resource_map WHERE id = $1;

-- name: RetrySatusehatResourceMap :exec
UPDATE connect.satusehat_resource_map
SET sync_status = 'pending', last_error = NULL, locked_until = NULL
WHERE id = $1 AND sync_status = 'failed';

-- name: InsertIntegrationRequestLog :exec
INSERT INTO connect.integration_request_log
    (company_id, merchant_id, provider_id, endpoint, request_payload, response_payload, http_status, status, duration_ms)
VALUES ($1, $2, (SELECT id FROM connect.integration_provider WHERE code = 'satusehat'),
        $3, $4, $5, $6, $7, $8);
