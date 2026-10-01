-- internal/db/queries/session.sql
-- core.refresh_token (2026-09-15-session-refresh-token-design.md)

-- name: GetCompanyMaxConcurrentSessions :one
SELECT max_concurrent_sessions FROM core.company WHERE id = $1;

-- name: CountActiveRefreshTokens :one
SELECT count(*) FROM core.refresh_token
WHERE user_id = $1 AND company_id = $2 AND revoked_at IS NULL AND expires_at > now();

-- name: ListActiveRefreshTokens :many
SELECT id, device_id, device_label, merchant_id, issued_at, last_used_at
FROM core.refresh_token
WHERE user_id = $1 AND revoked_at IS NULL AND expires_at > now()
ORDER BY last_used_at DESC;

-- name: CreateRefreshToken :one
INSERT INTO core.refresh_token
    (user_id, company_id, merchant_id, device_id, device_label, token_hash, expires_at, created_ip)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetRefreshTokenByHash :one
SELECT * FROM core.refresh_token WHERE token_hash = $1;

-- name: GetRefreshTokenByID :one
SELECT * FROM core.refresh_token WHERE id = $1;

-- name: RevokeRefreshToken :exec
UPDATE core.refresh_token SET revoked_at = now() WHERE id = $1 AND revoked_at IS NULL;

-- name: RevokeAllRefreshTokensForDevice :exec
UPDATE core.refresh_token SET revoked_at = now()
WHERE user_id = $1 AND device_id = $2 AND revoked_at IS NULL;

-- name: UpdateRefreshTokenMerchant :exec
UPDATE core.refresh_token SET merchant_id = $2 WHERE id = $1;

-- name: RevokeAllRefreshTokensForUserExcept :exec
-- token_hash is always a non-empty sha256 hex digest, so passing "" for the except
-- parameter (no current session, e.g. no refresh cookie on this request) revokes
-- every active row — see internal/session.RevokeAllExceptCurrent.
UPDATE core.refresh_token SET revoked_at = now()
WHERE user_id = $1 AND revoked_at IS NULL AND token_hash != $2;
