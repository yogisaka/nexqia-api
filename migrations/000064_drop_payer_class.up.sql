-- nexqia-api/migrations/000064_drop_payer_class.up.sql
-- Spec 2026-10-07-tariff-price-lists §3.4: service_rate is keyed by price_list_id now.
DROP INDEX IF EXISTS core.idx_service_rate_lookup;
ALTER TABLE core.service_rate DROP COLUMN payer_class;
