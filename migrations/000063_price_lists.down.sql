-- nexqia-api/migrations/000063_price_lists.down.sql
-- Fails if a service_item uses an item_type added by 000063 (no lossless mapping).
DELETE FROM core.template_application WHERE template_id IN (SELECT id FROM core.template WHERE kind = 'price_list');
DELETE FROM core.template WHERE kind = 'price_list';
DROP TABLE IF EXISTS core.template_price_item;
ALTER TABLE core.template DROP CONSTRAINT IF EXISTS chk_template_price_list_usage;
ALTER TABLE core.template DROP CONSTRAINT template_kind_check;
ALTER TABLE core.template ADD CONSTRAINT template_kind_check CHECK (kind IN ('role', 'department'));
DROP INDEX IF EXISTS core.idx_service_rate_list;
ALTER TABLE core.service_rate DROP COLUMN IF EXISTS price_list_id;
DROP TABLE IF EXISTS core.price_list_adjustment;
DROP TRIGGER IF EXISTS trg_price_list_one_level ON core.price_list;
DROP FUNCTION IF EXISTS core.trg_price_list_one_level();
DROP TABLE IF EXISTS core.price_list;
ALTER TABLE core.service_item DROP COLUMN IF EXISTS procedure_concept_id;
ALTER TABLE core.service_item DROP CONSTRAINT service_item_item_type_check;
ALTER TABLE core.service_item ADD CONSTRAINT service_item_item_type_check CHECK (item_type IN
    ('procedure','drug','material','room','package'));
