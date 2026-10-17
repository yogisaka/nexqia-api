-- internal/db/queries/onboarding.sql
-- Checklist status for the active merchant (spec 2026-10-17-onboarding-hub-checklist §3).

-- name: CheckOnboardingDepartments :one
SELECT EXISTS (SELECT 1 FROM core.department WHERE merchant_id = $1 AND deleted_at IS NULL) AS ok;

-- name: CheckOnboardingLocations :one
SELECT EXISTS (SELECT 1 FROM core.location WHERE merchant_id = $1 AND deleted_at IS NULL) AS ok;

-- name: CheckOnboardingPhysicians :one
SELECT EXISTS (SELECT 1 FROM core.physician WHERE merchant_id = $1 AND deleted_at IS NULL) AS ok;

-- name: CheckOnboardingSchedules :one
SELECT EXISTS (SELECT 1 FROM operations.physician_schedule WHERE merchant_id = $1 AND deleted_at IS NULL) AS ok;

-- name: CheckOnboardingStaffRoles :one
SELECT EXISTS (SELECT 1 FROM core.user_merchant_role WHERE merchant_id = $1 AND user_id <> $2) AS ok;

-- name: CheckOnboardingClinicalConfig :one
SELECT EXISTS (SELECT 1 FROM core.mrn_config WHERE merchant_id = $1) AS ok;

-- name: CheckOnboardingPayers :one
SELECT EXISTS (SELECT 1 FROM core.payer WHERE merchant_id = $1 AND deleted_at IS NULL) AS ok;
