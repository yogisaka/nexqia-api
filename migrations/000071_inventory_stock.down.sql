-- nexqia-api/migrations/000071_inventory_stock.down.sql
-- Revert spec 2026-10-07-inventory-17-stock-procurement §3 (#17a).

DELETE FROM core.role_permission WHERE permission_id IN (
    '00000000-0000-0000-0004-000000000039',
    '00000000-0000-0000-0004-000000000040',
    '00000000-0000-0000-0004-000000000041'
);
DELETE FROM core.template_role_permission WHERE permission_id IN (
    '00000000-0000-0000-0004-000000000039',
    '00000000-0000-0000-0004-000000000040',
    '00000000-0000-0000-0004-000000000041'
);
DELETE FROM core.permission WHERE id IN (
    '00000000-0000-0000-0004-000000000039',
    '00000000-0000-0000-0004-000000000040',
    '00000000-0000-0000-0004-000000000041'
);
UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

DROP TABLE inventory.stock_count_line, inventory.stock_count, inventory.stock_level, inventory.goods_receipt_line, inventory.goods_receipt, inventory.item_cost, inventory.stock_move, inventory.stock_balance, inventory.lot, inventory.merchant_setting;

ALTER TABLE core.location DROP CONSTRAINT location_functions_check;
ALTER TABLE core.location ADD CONSTRAINT location_functions_check
    CHECK (functions <@ ARRAY['practice','nurse_station','counter','ward_room','treatment','support']::text[]);

DELETE FROM core.supplier WHERE merchant_id IS NULL;
DROP INDEX core.uq_supplier_company_code;
DROP POLICY supplier_isolation ON core.supplier;
CREATE POLICY supplier_isolation ON core.supplier
    USING (merchant_id = current_setting('app.current_merchant_id')::uuid);
ALTER TABLE core.supplier ALTER COLUMN merchant_id SET NOT NULL;
