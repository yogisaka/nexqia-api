-- Intentionally a no-op: these rows may predate this migration (seed), and
-- removing permissions would silently strip access from existing roles.
SELECT 1;