-- nexqia-api/migrations/000061_email_claim_attempts.down.sql
-- Restores the 000052 two-parameter claim_emails. Rows already given up as
-- 'failed' by the three-parameter version stay failed.

DROP FUNCTION core.claim_emails(integer, integer, integer);

-- Claims due rows for one worker pass; SKIP LOCKED keeps concurrent API instances apart.
CREATE FUNCTION core.claim_emails(p_batch integer, p_lock_seconds integer)
RETURNS TABLE (id uuid, to_address text, subject text, body_text text, body_html text, attempts integer, kind text)
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    UPDATE core.email_outbox o
       SET status = 'sending', locked_until = now() + make_interval(secs => p_lock_seconds)
     WHERE o.id IN (
        SELECT e.id FROM core.email_outbox e
         WHERE (e.status = 'pending' AND e.next_attempt_at <= now())
            OR (e.status = 'sending' AND e.locked_until < now())
         ORDER BY e.next_attempt_at
         LIMIT p_batch
         FOR UPDATE SKIP LOCKED)
    RETURNING o.id, o.to_address, o.subject, o.body_text, o.body_html, o.attempts, o.kind;
$$;

REVOKE ALL ON FUNCTION core.claim_emails(integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.claim_emails(integer, integer) TO app_runtime;
