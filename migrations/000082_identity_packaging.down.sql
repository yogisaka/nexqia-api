DROP TABLE IF EXISTS inventory.repack_in_line;
DROP TABLE IF EXISTS inventory.repack_out_line;
DROP TABLE IF EXISTS inventory.repack;
DROP TABLE IF EXISTS inventory.serial;
DROP TABLE IF EXISTS inventory.consignment_usage;
-- Down = degradasi: baris konsinyasi beda supplier menyatu (spec §6.1).
DROP INDEX IF EXISTS uq_stock_balance_slot;
ALTER TABLE inventory.stock_balance DROP CONSTRAINT IF EXISTS chk_stock_balance_supplier;
ALTER TABLE inventory.stock_balance DROP COLUMN IF EXISTS supplier_id;
ALTER TABLE inventory.stock_balance ADD CONSTRAINT stock_balance_location_id_lot_id_source_status_key
    UNIQUE (location_id, lot_id, source, status);
ALTER TABLE inventory.stock_move DROP CONSTRAINT stock_move_move_type_check;
ALTER TABLE inventory.stock_move ADD CONSTRAINT stock_move_move_type_check
    CHECK (move_type IN ('receipt','issue','transfer_out','transfer_in','adjustment','count','status_change','reversal','supplier_return','destruction'));
ALTER TABLE inventory.item DROP COLUMN IF EXISTS track_serial;
