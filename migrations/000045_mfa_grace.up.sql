-- nexqia-api/migrations/000045_mfa_grace.up.sql
-- MFA grace period (tenant + platform) and platform admin TOTP,
-- see 2026-09-25-mfa-grace-enforcement-design.md.

ALTER TABLE core.app_user ADD COLUMN mfa_grace_until timestamptz;

ALTER TABLE platform.admin_user
    ADD COLUMN mfa_secret      text,
    ADD COLUMN mfa_grace_until timestamptz;

CREATE TABLE platform.admin_mfa_recovery_code (
    id             uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    admin_user_id  uuid NOT NULL REFERENCES platform.admin_user(id),
    code_hash      text NOT NULL,
    used_at        timestamptz,
    created_at     timestamptz NOT NULL DEFAULT now()
);

CREATE INDEX admin_mfa_recovery_code_user_active_idx
    ON platform.admin_mfa_recovery_code (admin_user_id, used_at);

GRANT SELECT, INSERT, UPDATE, DELETE ON platform.admin_mfa_recovery_code TO app_runtime;
