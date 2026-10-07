-- nexqia-api/migrations/000064_drop_payer_class.down.sql
ALTER TABLE core.service_rate ADD COLUMN payer_class text NOT NULL DEFAULT 'general';
UPDATE core.service_rate r SET payer_class = pl.code FROM core.price_list pl WHERE pl.id = r.price_list_id;
CREATE INDEX idx_service_rate_lookup ON core.service_rate
    (merchant_id, service_item_id, payer_class, effective_from) WHERE deleted_at IS NULL;
