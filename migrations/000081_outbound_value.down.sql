DELETE FROM core.template_role_permission trp
USING core.permission p
WHERE trp.permission_id = p.id AND p.code IN ('inventory.stock.destroy', 'inventory.stock.return');
DELETE FROM core.role_permission rp
USING core.permission p
WHERE rp.permission_id = p.id AND p.code IN ('inventory.stock.destroy', 'inventory.stock.return');
DELETE FROM core.permission WHERE code IN ('inventory.stock.destroy', 'inventory.stock.return');
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

DROP TABLE IF EXISTS inventory.inter_merchant_transfer_line;
DROP TABLE IF EXISTS inventory.inter_merchant_transfer;
DROP TABLE IF EXISTS inventory.destruction_line;
DROP TABLE IF EXISTS inventory.destruction;
DROP TABLE IF EXISTS inventory.supplier_return_line;
DROP TABLE IF EXISTS inventory.supplier_return;

-- The restored 8-value move_type CHECK rejects retur/pemusnahan ledger rows.
-- Their source documents (supplier_return, destruction) are dropped above and
-- the ledger is derived from them, so delete the derived stock_move rows.
DELETE FROM inventory.stock_move WHERE move_type IN ('supplier_return', 'destruction');

ALTER TABLE inventory.stock_move DROP CONSTRAINT stock_move_move_type_check;
ALTER TABLE inventory.stock_move ADD CONSTRAINT stock_move_move_type_check
    CHECK (move_type IN ('receipt','issue','transfer_out','transfer_in','adjustment','count','status_change','reversal'));
