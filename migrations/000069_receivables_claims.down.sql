-- nexqia-api/migrations/000069_receivables_claims.down.sql
UPDATE core.template SET version = version - 1
WHERE scope = 'platform' AND kind = 'role' AND code IN ('klinik_pratama', 'klinik_utama', 'rumah_sakit');
DELETE FROM core.template_role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000037';
DELETE FROM core.role_permission
WHERE permission_id = '00000000-0000-0000-0004-000000000037';
DELETE FROM core.permission WHERE id = '00000000-0000-0000-0004-000000000037';
DROP TABLE operations.receivable_adjustment, operations.payer_payment_allocation, operations.payer_payment, operations.receivable, operations.claim_batch;
ALTER TABLE core.payer DROP COLUMN claim_documents, DROP COLUMN payment_term_days;
