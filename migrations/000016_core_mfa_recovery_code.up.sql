-- migrations/000016_core_mfa_recovery_code.up.sql
-- TOTP recovery/backup codes, see docs/design/specs/2026-09-15-totp-2fa-design.md §7.
-- core.app_user.mfa_secret already exists (000004_core_rbac) — no new column needed there.

CREATE TABLE core.mfa_recovery_code (
    id          uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    user_id     uuid NOT NULL REFERENCES core.app_user(id),
    company_id  uuid NOT NULL REFERENCES core.company(id),
    code_hash   text NOT NULL,
    used_at     timestamptz,
    created_at  timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX mfa_recovery_code_user_active_idx ON core.mfa_recovery_code (user_id, used_at);
ALTER TABLE core.mfa_recovery_code ENABLE ROW LEVEL SECURITY;
CREATE POLICY mfa_recovery_code_isolation ON core.mfa_recovery_code
    USING (company_id = current_setting('app.current_company_id')::uuid);
