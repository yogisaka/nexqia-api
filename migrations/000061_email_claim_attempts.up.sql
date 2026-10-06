-- nexqia-api/migrations/000061_email_claim_attempts.up.sql
-- Spec 2026-10-06-email-outbox-claim §2.2: a 'sending' row whose lock ran out
-- means the previous attempt died mid-send. Re-claiming it now counts as an
-- attempt (attempts+1, last_error set); at p_max_attempts the row is given up
-- as 'failed' and not returned, instead of being retried forever.

DROP FUNCTION core.claim_emails(integer, integer);

CREATE FUNCTION core.claim_emails(p_batch integer, p_lock_seconds integer, p_max_attempts integer)
RETURNS TABLE (id uuid, to_address text, subject text, body_text text, body_html text, attempts integer, kind text)
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    WITH picked AS (
        SELECT e.id, e.status = 'sending' AS stale
          FROM core.email_outbox e
         WHERE (e.status = 'pending' AND e.next_attempt_at <= now())
            OR (e.status = 'sending' AND e.locked_until < now())
         ORDER BY e.next_attempt_at
         LIMIT p_batch
           FOR UPDATE SKIP LOCKED
    ), claimed AS (
        UPDATE core.email_outbox o
           SET attempts     = o.attempts + CASE WHEN p.stale THEN 1 ELSE 0 END,
               last_error   = CASE WHEN p.stale THEN 'worker stopped during send (lock expired)' ELSE o.last_error END,
               status       = CASE WHEN p.stale AND o.attempts + 1 >= p_max_attempts THEN 'failed' ELSE 'sending' END,
               locked_until = CASE WHEN p.stale AND o.attempts + 1 >= p_max_attempts THEN NULL
                                   ELSE now() + make_interval(secs => p_lock_seconds) END
          FROM picked p
         WHERE o.id = p.id
      RETURNING o.id, o.to_address, o.subject, o.body_text, o.body_html, o.attempts, o.kind, o.status
    )
    SELECT c.id, c.to_address, c.subject, c.body_text, c.body_html, c.attempts, c.kind
      FROM claimed c
     WHERE c.status = 'sending';
$$;

REVOKE ALL ON FUNCTION core.claim_emails(integer, integer, integer) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.claim_emails(integer, integer, integer) TO app_runtime;
