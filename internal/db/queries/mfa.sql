-- internal/db/queries/mfa.sql
-- core.mfa_recovery_code (2026-09-15-totp-2fa-design.md §7)

-- name: CreateMFARecoveryCode :exec
INSERT INTO core.mfa_recovery_code (user_id, company_id, code_hash)
VALUES ($1, $2, $3);

-- name: DeleteMFARecoveryCodesForUser :exec
DELETE FROM core.mfa_recovery_code WHERE user_id = $1;

-- name: ListActiveMFARecoveryCodes :many
SELECT * FROM core.mfa_recovery_code WHERE user_id = $1 AND used_at IS NULL;

-- name: MarkMFARecoveryCodeUsed :exec
UPDATE core.mfa_recovery_code SET used_at = now() WHERE id = $1;
