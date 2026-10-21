DELETE FROM core.template_role_permission trp
USING core.permission p
WHERE trp.permission_id = p.id AND p.code IN ('inventory.coldchain.log', 'inventory.emergency.use');
DELETE FROM core.role_permission rp
USING core.permission p
WHERE rp.permission_id = p.id AND p.code IN ('inventory.coldchain.log', 'inventory.emergency.use');
DELETE FROM core.permission WHERE code IN ('inventory.coldchain.log', 'inventory.emergency.use');
UPDATE core.template SET version = version + 1
WHERE scope = 'platform' AND kind = 'role'
  AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');

DROP TABLE IF EXISTS inventory.emergency_kit_seal;
DROP TABLE IF EXISTS inventory.emergency_kit_item;
DROP TABLE IF EXISTS inventory.emergency_kit;
DROP TABLE IF EXISTS inventory.fridge_temp_log;
DROP TABLE IF EXISTS inventory.fridge;
DROP TABLE IF EXISTS inventory.vvm_log;
DROP TABLE IF EXISTS inventory.opened_container;
