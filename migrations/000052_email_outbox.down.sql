DROP FUNCTION IF EXISTS core.user_device_seen(uuid, text);
DROP FUNCTION IF EXISTS core.purge_emails(timestamptz);
DROP FUNCTION IF EXISTS core.mark_email_failed(uuid, text, timestamptz, integer);
DROP FUNCTION IF EXISTS core.mark_email_sent(uuid);
DROP FUNCTION IF EXISTS core.claim_emails(integer, integer);
DROP FUNCTION IF EXISTS core.enqueue_email(uuid, text, text, text, text, text);
DROP TABLE IF EXISTS core.email_outbox;
