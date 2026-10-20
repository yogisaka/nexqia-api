-- internal/db/queries/connect.sql
-- Connect pillar (Permenkes 6/2026 Fase C1-C3, spec
-- 2026-10-20-permenkes-6-2026-compliance-design): all integration queries live
-- here. The worker reads satusehat_resource_map ONLY through the SECURITY
-- DEFINER claim function — RLS tenant GUC never sticks to pool connections.

-- name: ClaimSatusehatMaps :many
SELECT * FROM connect.claim_satusehat_maps($1, $2, $3);

-- name: MarkSatusehatMapSynced :exec
UPDATE connect.satusehat_resource_map
SET sync_status = 'synced', fhir_resource_id = $2, last_synced_at = now(),
    last_error = NULL, locked_until = NULL
WHERE id = $1;

-- name: MarkSatusehatMapFailed :exec
UPDATE connect.satusehat_resource_map
SET sync_status = 'failed', last_error = $2, locked_until = NULL
WHERE id = $1;

-- name: InsertIntegrationRequestLog :exec
INSERT INTO connect.integration_request_log
    (company_id, merchant_id, provider_id, endpoint, request_payload, response_payload, http_status, status, duration_ms)
VALUES ($1, $2, (SELECT id FROM connect.integration_provider WHERE code = 'satusehat'),
        $3, $4, $5, $6, $7, $8);
