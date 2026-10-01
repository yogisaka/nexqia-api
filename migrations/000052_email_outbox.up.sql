-- nexqia-api/migrations/000052_email_outbox.up.sql
-- #32 transactional email outbox (spec 2026-10-01-email-outbox-design §3.1).
-- Rows are reachable only through the SECURITY DEFINER functions below:
-- registration runs before the tenant GUC is set and the worker serves all companies.

CREATE TABLE core.email_outbox (
    id              uuid PRIMARY KEY DEFAULT uuid_generate_v7(),
    company_id      uuid REFERENCES core.company(id),
    kind            text NOT NULL CHECK (kind IN ('welcome', 'new_device_login')),
    to_address      text NOT NULL CHECK (btrim(to_address) <> ''),
    subject         text NOT NULL,
    body_text       text NOT NULL,
    body_html       text NOT NULL,
    status          text NOT NULL DEFAULT 'pending' CHECK (status IN ('pending', 'sending', 'sent', 'failed')),
    attempts        integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until    timestamptz,
    last_error      text,
    created_at      timestamptz NOT NULL DEFAULT now(),
    sent_at         timestamptz
);
CREATE INDEX idx_email_outbox_due ON core.email_outbox (status, next_attempt_at);

ALTER TABLE core.email_outbox ENABLE ROW LEVEL SECURITY;
REVOKE ALL ON core.email_outbox FROM app_runtime;

CREATE FUNCTION core.enqueue_email(p_company_id uuid, p_kind text, p_to text, p_subject text, p_text text, p_html text)
RETURNS uuid
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    INSERT INTO core.email_outbox (company_id, kind, to_address, subject, body_text, body_html)
    VALUES (p_company_id, p_kind, p_to, p_subject, p_text, p_html)
    RETURNING id;
$$;

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

CREATE FUNCTION core.mark_email_sent(p_id uuid)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    UPDATE core.email_outbox
       SET status = 'sent', sent_at = now(), locked_until = NULL, last_error = NULL
     WHERE id = p_id;
$$;

-- attempts+1; at p_max_attempts the row is given up as 'failed', else retried at p_retry_at.
CREATE FUNCTION core.mark_email_failed(p_id uuid, p_error text, p_retry_at timestamptz, p_max_attempts integer)
RETURNS void
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    UPDATE core.email_outbox
       SET attempts        = attempts + 1,
           status          = CASE WHEN attempts + 1 >= p_max_attempts THEN 'failed' ELSE 'pending' END,
           next_attempt_at = p_retry_at,
           locked_until    = NULL,
           last_error      = left(p_error, 500)
     WHERE id = p_id;
$$;

CREATE FUNCTION core.purge_emails(p_older_than timestamptz)
RETURNS integer
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
AS $$
    WITH d AS (
        DELETE FROM core.email_outbox
         WHERE status IN ('sent', 'failed') AND created_at < p_older_than
        RETURNING 1)
    SELECT count(*)::integer FROM d;
$$;

-- Has this user ever had a session on this device (any status)? core.refresh_token
-- RLS needs the company GUC, which login has not necessarily set yet.
CREATE FUNCTION core.user_device_seen(p_user_id uuid, p_device_id text)
RETURNS boolean
LANGUAGE sql
SECURITY DEFINER
SET search_path = core, pg_temp
STABLE
AS $$
    SELECT EXISTS (SELECT 1 FROM core.refresh_token WHERE user_id = p_user_id AND device_id = p_device_id);
$$;

REVOKE ALL ON FUNCTION core.enqueue_email(uuid, text, text, text, text, text) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.claim_emails(integer, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.mark_email_sent(uuid) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.mark_email_failed(uuid, text, timestamptz, integer) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.purge_emails(timestamptz) FROM PUBLIC;
REVOKE ALL ON FUNCTION core.user_device_seen(uuid, text) FROM PUBLIC;
GRANT EXECUTE ON FUNCTION core.enqueue_email(uuid, text, text, text, text, text) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.claim_emails(integer, integer) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.mark_email_sent(uuid) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.mark_email_failed(uuid, text, timestamptz, integer) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.purge_emails(timestamptz) TO app_runtime;
GRANT EXECUTE ON FUNCTION core.user_device_seen(uuid, text) TO app_runtime;
