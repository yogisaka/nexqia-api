//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/mail"
)

// beginAsRuntime opens a transaction on the owner pool and switches it to
// app_runtime, the role the API runs as. Rolled back on test cleanup unless
// the test commits it first.
func beginAsRuntime(t *testing.T, ctx context.Context, pool *pgxpool.Pool) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	t.Cleanup(func() { tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set role app_runtime: %v", err)
	}
	return tx
}

func enqueueTestEmail(t *testing.T, ctx context.Context, q *sqlcgen.Queries, to string) pgtype.UUID {
	t.Helper()
	id, err := q.EnqueueEmail(ctx, sqlcgen.EnqueueEmailParams{
		Kind: "welcome", ToAddress: to, Subject: "Subjek", BodyText: "teks", BodyHtml: "<p>html</p>",
	})
	if err != nil {
		t.Fatalf("enqueue email: %v", err)
	}
	return id
}

func outboxRow(t *testing.T, ctx context.Context, pool *pgxpool.Pool, id pgtype.UUID) (status string, attempts int32, nextAttempt time.Time, lastError *string) {
	t.Helper()
	err := pool.QueryRow(ctx,
		"SELECT status, attempts, next_attempt_at, last_error FROM core.email_outbox WHERE id = $1", id,
	).Scan(&status, &attempts, &nextAttempt, &lastError)
	if err != nil {
		t.Fatalf("read outbox row: %v", err)
	}
	return
}

func TestEmailOutbox_DB_RuntimeCannotReadTable(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	tx := beginAsRuntime(t, ctx, pool)
	_, err := tx.Exec(ctx, "SELECT * FROM core.email_outbox")
	if err == nil || !strings.Contains(err.Error(), "permission denied") {
		t.Fatalf("expected permission denied for app_runtime SELECT, got %v", err)
	}
}

func TestEmailOutbox_DB_EnqueueClaimMark(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	tx := beginAsRuntime(t, ctx, pool)
	q := sqlcgen.New(tx)
	id := enqueueTestEmail(t, ctx, q, "andi@example.com")

	claimed, err := mail.ClaimEmails(ctx, tx, 10, 120)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != id {
		t.Fatalf("expected the enqueued row claimed, got %+v", claimed)
	}
	c := claimed[0]
	if c.To != "andi@example.com" || c.Subject != "Subjek" || c.Text != "teks" || c.HTML != "<p>html</p>" || c.Kind != "welcome" || c.Attempts != 0 {
		t.Fatalf("unexpected claimed row: %+v", c)
	}
	if err := q.MarkEmailSent(ctx, id); err != nil {
		t.Fatalf("mark sent: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}

	var status string
	var sentAt *time.Time
	if err := pool.QueryRow(ctx, "SELECT status, sent_at FROM core.email_outbox WHERE id = $1", id).Scan(&status, &sentAt); err != nil {
		t.Fatalf("read row: %v", err)
	}
	if status != "sent" || sentAt == nil {
		t.Fatalf("expected sent with sent_at, got %s %v", status, sentAt)
	}

	again, err := mail.ClaimEmails(ctx, pool, 10, 120)
	if err != nil {
		t.Fatalf("claim again: %v", err)
	}
	if len(again) != 0 {
		t.Fatalf("sent row must not be claimed again, got %d", len(again))
	}
}

func TestEmailOutbox_DB_ConcurrentClaimNoOverlap(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	q := sqlcgen.New(pool)
	for i := 0; i < 4; i++ {
		enqueueTestEmail(t, ctx, q, "user@example.com")
	}

	tx1 := beginAsRuntime(t, ctx, pool)
	first, err := mail.ClaimEmails(ctx, tx1, 2, 120)
	if err != nil {
		t.Fatalf("claim tx1: %v", err)
	}
	// tx1 still open: its rows are row-locked, the second claim must skip them.
	tx2 := beginAsRuntime(t, ctx, pool)
	second, err := mail.ClaimEmails(ctx, tx2, 10, 120)
	if err != nil {
		t.Fatalf("claim tx2: %v", err)
	}
	if len(first) != 2 || len(second) != 2 {
		t.Fatalf("expected 2+2 claimed rows, got %d+%d", len(first), len(second))
	}
	seen := map[pgtype.UUID]bool{}
	for _, e := range append(first, second...) {
		if seen[e.ID] {
			t.Fatalf("row %v claimed by both transactions", e.ID)
		}
		seen[e.ID] = true
	}
}

func TestEmailOutbox_DB_FailRetryThenGiveUp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	q := sqlcgen.New(pool)
	id := enqueueTestEmail(t, ctx, q, "andi@example.com")

	retryAt := time.Now().Add(2 * time.Minute)
	if err := q.MarkEmailFailed(ctx, sqlcgen.MarkEmailFailedParams{
		ID: id, Error: "dial tcp: timeout", RetryAt: pgtype.Timestamptz{Time: retryAt, Valid: true}, MaxAttempts: 2,
	}); err != nil {
		t.Fatalf("mark failed 1: %v", err)
	}
	status, attempts, next, lastErr := outboxRow(t, ctx, pool, id)
	if status != "pending" || attempts != 1 || lastErr == nil || *lastErr != "dial tcp: timeout" {
		t.Fatalf("after first failure expected pending/1/error, got %s/%d/%v", status, attempts, lastErr)
	}
	if next.Sub(retryAt).Abs() > time.Second {
		t.Fatalf("next_attempt_at %v, want %v", next, retryAt)
	}
	claimed, err := mail.ClaimEmails(ctx, pool, 10, 120)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 0 {
		t.Fatalf("row must not be claimed before next_attempt_at, got %d", len(claimed))
	}

	if err := q.MarkEmailFailed(ctx, sqlcgen.MarkEmailFailedParams{
		ID: id, Error: strings.Repeat("x", 600), RetryAt: pgtype.Timestamptz{Time: retryAt, Valid: true}, MaxAttempts: 2,
	}); err != nil {
		t.Fatalf("mark failed 2: %v", err)
	}
	status, attempts, _, lastErr = outboxRow(t, ctx, pool, id)
	if status != "failed" || attempts != 2 {
		t.Fatalf("after reaching max attempts expected failed/2, got %s/%d", status, attempts)
	}
	if lastErr == nil || len(*lastErr) != 500 {
		t.Fatalf("last_error must be cut to 500 characters, got %d", len(*lastErr))
	}
}

func TestEmailOutbox_DB_ReclaimStaleSending(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	q := sqlcgen.New(pool)
	stale := enqueueTestEmail(t, ctx, q, "stale@example.com")
	fresh := enqueueTestEmail(t, ctx, q, "fresh@example.com")

	// A worker died mid-send: stale's lock has run out, fresh's has not.
	if _, err := pool.Exec(ctx, "UPDATE core.email_outbox SET status = 'sending', locked_until = now() - interval '1 minute' WHERE id = $1", stale); err != nil {
		t.Fatalf("seed stale: %v", err)
	}
	if _, err := pool.Exec(ctx, "UPDATE core.email_outbox SET status = 'sending', locked_until = now() + interval '1 minute' WHERE id = $1", fresh); err != nil {
		t.Fatalf("seed fresh: %v", err)
	}

	claimed, err := mail.ClaimEmails(ctx, pool, 10, 120)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if len(claimed) != 1 || claimed[0].ID != stale {
		t.Fatalf("expected only the stale row reclaimed, got %+v", claimed)
	}
}

func TestEmailOutbox_DB_PurgeOld(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	q := sqlcgen.New(pool)
	oldSent := enqueueTestEmail(t, ctx, q, "a@example.com")
	oldFailed := enqueueTestEmail(t, ctx, q, "b@example.com")
	oldPending := enqueueTestEmail(t, ctx, q, "c@example.com")
	newSent := enqueueTestEmail(t, ctx, q, "d@example.com")

	for _, s := range []struct {
		id     pgtype.UUID
		status string
		age    string
	}{
		{oldSent, "sent", "40 days"}, {oldFailed, "failed", "40 days"}, {oldPending, "pending", "40 days"}, {newSent, "sent", "1 day"},
	} {
		if _, err := pool.Exec(ctx, "UPDATE core.email_outbox SET status = $2, created_at = now() - $3::interval WHERE id = $1", s.id, s.status, s.age); err != nil {
			t.Fatalf("seed row: %v", err)
		}
	}

	tx := beginAsRuntime(t, ctx, pool)
	deleted, err := sqlcgen.New(tx).PurgeEmails(ctx, pgtype.Timestamptz{Time: time.Now().AddDate(0, 0, -30), Valid: true})
	if err != nil {
		t.Fatalf("purge: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	if deleted != 2 {
		t.Fatalf("expected 2 rows purged, got %d", deleted)
	}
	var left int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.email_outbox WHERE id = ANY($1)", []pgtype.UUID{oldPending, newSent}).Scan(&left); err != nil {
		t.Fatalf("count: %v", err)
	}
	if left != 2 {
		t.Fatalf("old pending and recent sent rows must stay, got %d", left)
	}
}

func TestEmailOutbox_DB_DeviceSeen(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	companyID, merchantID, userID := setupPinLockTestUser(t, ctx, pool, testHasher(), "", false, 5)
	if _, err := pool.Exec(ctx,
		`INSERT INTO core.refresh_token (user_id, company_id, merchant_id, device_id, device_label, token_hash, expires_at, created_ip, revoked_at)
		 VALUES ($1, $2, $3, 'known-device', 'Chrome', 'hash-1', now() + interval '1 day', '127.0.0.1', now())`,
		userID, companyID, merchantID); err != nil {
		t.Fatalf("seed refresh_token: %v", err)
	}

	var uid pgtype.UUID
	if err := uid.Scan(userID); err != nil {
		t.Fatalf("parse user id: %v", err)
	}
	// No company GUC set: the definer function must still see the (revoked) row.
	tx := beginAsRuntime(t, ctx, pool)
	q := sqlcgen.New(tx)
	seen, err := q.UserDeviceSeen(ctx, sqlcgen.UserDeviceSeenParams{UserID: uid, DeviceID: "known-device"})
	if err != nil {
		t.Fatalf("device seen: %v", err)
	}
	if !seen {
		t.Fatalf("expected known-device seen")
	}
	seen, err = q.UserDeviceSeen(ctx, sqlcgen.UserDeviceSeenParams{UserID: uid, DeviceID: "new-device"})
	if err != nil {
		t.Fatalf("device seen: %v", err)
	}
	if seen {
		t.Fatalf("expected new-device not seen")
	}
}
