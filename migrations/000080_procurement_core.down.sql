DELETE FROM core.template_role_permission trp
USING core.permission p
WHERE trp.permission_id = p.id
  AND p.code IN ('inventory.purchase.request', 'inventory.purchase.approve', 'inventory.purchase.manage');
DELETE FROM core.role_permission rp
USING core.permission p
WHERE rp.permission_id = p.id
  AND p.code IN ('inventory.purchase.request', 'inventory.purchase.approve', 'inventory.purchase.manage');
DELETE FROM core.permission WHERE code IN ('inventory.purchase.request', 'inventory.purchase.approve', 'inventory.purchase.manage');
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

DROP INDEX IF EXISTS idx_goods_receipt_po;
ALTER TABLE inventory.goods_receipt_line DROP COLUMN IF EXISTS purchase_order_line_id;
ALTER TABLE inventory.goods_receipt DROP COLUMN IF EXISTS purchase_order_id;
DROP TABLE IF EXISTS inventory.purchase_order_line;
DROP TABLE IF EXISTS inventory.purchase_order;
DROP TABLE IF EXISTS inventory.purchase_request_line;
DROP TABLE IF EXISTS inventory.purchase_request;
DROP TABLE IF EXISTS inventory.contract_price;
DROP TABLE IF EXISTS inventory.procurement_central_item;
DROP TABLE IF EXISTS inventory.procurement_setting;
