BEGIN;

DROP FUNCTION IF EXISTS core.scan_credential_expiry(date);
DROP FUNCTION IF EXISTS core.credential_admin_emails(uuid);
DROP FUNCTION IF EXISTS core.credential_merchant_timezone(uuid);
DROP FUNCTION IF EXISTS core.enqueue_credential_email(uuid, text, text, text, text, text);
DROP INDEX IF EXISTS uq_email_outbox_dedup_key;
ALTER TABLE core.email_outbox DROP COLUMN IF EXISTS dedup_key;
ALTER TABLE core.email_outbox DROP CONSTRAINT IF EXISTS email_outbox_kind_check;
DELETE FROM core.email_outbox WHERE kind = 'staff_credential_expiry';
ALTER TABLE core.email_outbox ADD CONSTRAINT email_outbox_kind_check
    CHECK (kind IN ('welcome', 'new_device_login'));
DROP TABLE IF EXISTS core.staff_credential;

COMMIT;
