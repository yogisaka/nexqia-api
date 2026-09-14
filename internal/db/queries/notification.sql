-- internal/db/queries/notification.sql
-- core.notification (docs/07-core-ddl.md §12 "Notification")

-- name: CreateNotification :one
INSERT INTO core.notification (company_id, merchant_id, channel, reference_table, reference_id, message, payload, created_by)
VALUES ($1, $2, $3, $4, $5, $6, $7, $8)
RETURNING *;

-- name: GetNotificationByID :one
SELECT * FROM core.notification WHERE id = $1;

-- name: ListNotificationsByMerchant :many
SELECT * FROM core.notification
WHERE merchant_id = $1 AND channel = $2
ORDER BY created_at DESC
LIMIT $3 OFFSET $4;

-- name: ListUnreadNotifications :many
SELECT * FROM core.notification
WHERE merchant_id = $1 AND NOT is_read
ORDER BY created_at DESC
LIMIT $2;

-- name: MarkNotificationRead :exec
UPDATE core.notification SET is_read = true WHERE id = $1;

-- name: MarkAllNotificationsRead :exec
UPDATE core.notification SET is_read = true WHERE merchant_id = $1 AND channel = $2 AND NOT is_read;

-- name: DeleteNotification :exec
DELETE FROM core.notification WHERE id = $1;
