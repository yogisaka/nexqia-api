-- nexqia-api/migrations/000070_practitioner_fees_tax.down.sql

UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
DELETE FROM core.template_role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000038';
DELETE FROM core.role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000038';
DELETE FROM core.permission WHERE id = '00000000-0000-0000-0004-000000000038';
DROP TABLE core.tax_setting, core.tax_pasal17_bracket, core.tax_ptkp, core.tax_ter_rate,
    operations.fee_payout_line, operations.practitioner_fee, operations.fee_payout_batch,
    core.practitioner_fee_rule, operations.charge_performer;
ALTER TABLE operations.charge_split DROP COLUMN components;
ALTER TABLE core.physician DROP CONSTRAINT chk_physician_ptkp, DROP COLUMN npwp, DROP COLUMN ptkp_status, DROP COLUMN employment_type, DROP COLUMN profession;
