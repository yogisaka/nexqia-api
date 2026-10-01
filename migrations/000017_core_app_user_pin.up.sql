-- migrations/000017_core_app_user_pin.up.sql
-- PIN-unlock (app-lock), see 2026-09-15-pin-unlock-design.md §5.
-- Nullable: NULL means the user hasn't enrolled a PIN (lock not enforced for them, §3).
ALTER TABLE core.app_user ADD COLUMN pin_hash text;
