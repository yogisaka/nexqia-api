-- name: EnqueueEmail :one
SELECT core.enqueue_email(sqlc.narg(company_id)::uuid, sqlc.arg(kind)::text, sqlc.arg(to_address)::text,
    sqlc.arg(subject)::text, sqlc.arg(body_text)::text, sqlc.arg(body_html)::text)::uuid AS id;

-- core.claim_emails (RETURNS TABLE) is called as a raw query in internal/mail
-- (ClaimEmails): sqlc can't type its result columns.

-- name: MarkEmailSent :exec
SELECT core.mark_email_sent(sqlc.arg(id)::uuid);

-- name: MarkEmailFailed :exec
SELECT core.mark_email_failed(sqlc.arg(id)::uuid, sqlc.arg(error)::text, sqlc.arg(retry_at)::timestamptz, sqlc.arg(max_attempts)::integer);

-- name: PurgeEmails :one
SELECT core.purge_emails(sqlc.arg(older_than)::timestamptz)::integer AS deleted;

-- name: UserDeviceSeen :one
SELECT core.user_device_seen(sqlc.arg(user_id)::uuid, sqlc.arg(device_id)::text)::boolean AS seen;
