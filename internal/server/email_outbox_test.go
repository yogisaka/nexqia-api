//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/mail"
	"github.com/yogisaka/nexqia-api/internal/server"
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

	claimed, err := mail.ClaimEmails(ctx, tx, 10, 120, 5)
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

	again, err := mail.ClaimEmails(ctx, pool, 10, 120, 5)
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
	first, err := mail.ClaimEmails(ctx, tx1, 2, 120, 5)
	if err != nil {
		t.Fatalf("claim tx1: %v", err)
	}
	// tx1 still open: its rows are row-locked, the second claim must skip them.
	tx2 := beginAsRuntime(t, ctx, pool)
	second, err := mail.ClaimEmails(ctx, tx2, 10, 120, 5)
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
	claimed, err := mail.ClaimEmails(ctx, pool, 10, 120, 5)
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

	claimed, err := mail.ClaimEmails(ctx, pool, 10, 120, 5)
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

// --- Registration / login triggers and the worker (Task 3) ---

func emailTestRouter(t *testing.T, ctx context.Context) (*pgxpool.Pool, http.Handler) {
	t.Helper()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	cfg := testConfig()
	cfg.AppBaseURL = "https://app.example.test"
	return pool, server.NewRouter(pool, redisClient, cfg)
}

func outboxCount(t *testing.T, ctx context.Context, pool *pgxpool.Pool, kind string) int {
	t.Helper()
	var n int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.email_outbox WHERE kind = $1", kind).Scan(&n); err != nil {
		t.Fatalf("count outbox: %v", err)
	}
	return n
}

// registerForEmail registers an owner from deviceID, then logs that session out
// so later logins don't run into the device limit. Returns company ID and code.
func registerForEmail(t *testing.T, router http.Handler, username, email, phone, deviceID string) (companyID, companyCode string) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload(username, email, phone)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", deviceID)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			CompanyID   string `json:"company_id"`
			CompanyCode string `json:"company_code"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register: %v", err)
	}
	logoutFrom(t, router, resp.Data.CompanyID, rec)
	return resp.Data.CompanyID, resp.Data.CompanyCode
}

func logoutFrom(t *testing.T, router http.Handler, companyID string, rec *httptest.ResponseRecorder) {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/logout", nil)
	req.Header.Set("X-Company-ID", companyID)
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "refresh_token" {
			req.AddCookie(ck)
		}
	}
	out := httptest.NewRecorder()
	router.ServeHTTP(out, req)
	if out.Code != http.StatusOK {
		t.Fatalf("logout expected 200, got %d: %s", out.Code, out.Body.String())
	}
}

// loginFrom logs username in from deviceID, asserts 200 and logs out again.
func loginFrom(t *testing.T, router http.Handler, companyID, username, deviceID string) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"username": username, "password": "Passw0rd123!"})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/login", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Company-ID", companyID)
	req.Header.Set("X-Device-Id", deviceID)
	req.Header.Set("User-Agent", "Firefox di Linux")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("login expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			Token string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || resp.Data.Token == "" {
		t.Fatalf("login must return a session token, got %s", rec.Body.String())
	}
	logoutFrom(t, router, companyID, rec)
}

func TestEmailOutbox_RegisterEnqueuesWelcome(t *testing.T) {
	ctx := context.Background()
	pool, router := emailTestRouter(t, ctx)

	_, code := registerForEmail(t, router, "welcome.owner", "welcome.owner@example.com", "081234500201", "dev-reg")

	var to, subject, text, html string
	if err := pool.QueryRow(ctx,
		"SELECT to_address, subject, body_text, body_html FROM core.email_outbox WHERE kind = 'welcome'",
	).Scan(&to, &subject, &text, &html); err != nil {
		t.Fatalf("expected exactly one welcome row: %v", err)
	}
	if outboxCount(t, ctx, pool, "welcome") != 1 {
		t.Fatalf("expected exactly one welcome row")
	}
	if to != "welcome.owner@example.com" || subject != "Selamat datang di NEXQIA — kode perusahaan Anda" {
		t.Fatalf("unexpected welcome row: to=%q subject=%q", to, subject)
	}
	for _, body := range []string{text, html} {
		if !strings.Contains(body, code) || !strings.Contains(body, "https://app.example.test/login") || !strings.Contains(body, "welcome.owner") {
			t.Fatalf("welcome body must carry company code %q, login URL and username: %q", code, body)
		}
	}
	if strings.Contains(text, "Passw0rd123!") || strings.Contains(html, "Passw0rd123!") {
		t.Fatalf("welcome email must never contain the password")
	}
	// The registration device is now known: no new-device alert from registering.
	if n := outboxCount(t, ctx, pool, "new_device_login"); n != 0 {
		t.Fatalf("registration must not queue a new-device alert, got %d", n)
	}
}

func TestEmailOutbox_RegisterConflictNoEmail(t *testing.T) {
	ctx := context.Background()
	pool, router := emailTestRouter(t, ctx)
	registerForEmail(t, router, "conflict.one", "conflict@example.com", "081234500202", "dev-1")

	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(registerPayload("conflict.two", "conflict@example.com", "081234500203")))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "dev-2")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("duplicate email expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	if n := outboxCount(t, ctx, pool, "welcome"); n != 1 {
		t.Fatalf("rejected registration must not queue a welcome email, got %d rows", n)
	}
}

func TestEmailOutbox_LoginSameDeviceNoAlert(t *testing.T) {
	ctx := context.Background()
	pool, router := emailTestRouter(t, ctx)
	companyID, _ := registerForEmail(t, router, "same.device", "same.device@example.com", "081234500204", "dev-home")

	loginFrom(t, router, companyID, "same.device", "dev-home")
	if n := outboxCount(t, ctx, pool, "new_device_login"); n != 0 {
		t.Fatalf("login from the registration device must not alert, got %d", n)
	}
}

func TestEmailOutbox_LoginNewDeviceAlerts(t *testing.T) {
	ctx := context.Background()
	pool, router := emailTestRouter(t, ctx)
	companyID, _ := registerForEmail(t, router, "new.device", "new.device@example.com", "081234500205", "dev-home")

	loginFrom(t, router, companyID, "new.device", "dev-office")
	if n := outboxCount(t, ctx, pool, "new_device_login"); n != 1 {
		t.Fatalf("login from a new device expected 1 alert, got %d", n)
	}
	var to, text string
	if err := pool.QueryRow(ctx, "SELECT to_address, body_text FROM core.email_outbox WHERE kind = 'new_device_login'").Scan(&to, &text); err != nil {
		t.Fatalf("read alert: %v", err)
	}
	if to != "new.device@example.com" || !strings.Contains(text, "Firefox di Linux") ||
		!strings.Contains(text, "https://app.example.test/account/security") || !strings.Contains(text, "WIB") {
		t.Fatalf("unexpected alert: to=%q text=%q", to, text)
	}

	loginFrom(t, router, companyID, "new.device", "dev-office")
	if n := outboxCount(t, ctx, pool, "new_device_login"); n != 1 {
		t.Fatalf("second login from the same device must not alert again, got %d", n)
	}
}

func TestEmailOutbox_LoginAlertFailureDoesNotBlockLogin(t *testing.T) {
	ctx := context.Background()
	pool, router := emailTestRouter(t, ctx)
	companyID, _ := registerForEmail(t, router, "broken.mail", "broken.mail@example.com", "081234500206", "dev-home")
	// ' ' passes the Go non-empty check but violates CHECK (btrim(to_address) <> ''),
	// so the enqueue fails inside the login transaction.
	if _, err := pool.Exec(ctx, "UPDATE core.app_user SET email = ' ' WHERE username = 'broken.mail'"); err != nil {
		t.Fatalf("blank email: %v", err)
	}

	loginFrom(t, router, companyID, "broken.mail", "dev-office")
	if n := outboxCount(t, ctx, pool, "new_device_login"); n != 0 {
		t.Fatalf("failed alert must leave no row, got %d", n)
	}
	// The login transaction committed despite the failed enqueue: the session exists.
	var sessions int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.refresh_token WHERE device_id = 'dev-office'").Scan(&sessions); err != nil {
		t.Fatalf("count sessions: %v", err)
	}
	if sessions != 1 {
		t.Fatalf("login session must be committed, got %d refresh tokens", sessions)
	}
}

func TestEmailOutbox_UserWithoutEmailNoAlert(t *testing.T) {
	ctx := context.Background()
	pool, router := emailTestRouter(t, ctx)
	companyID, _ := registerForEmail(t, router, "no.mail", "no.mail@example.com", "081234500207", "dev-home")
	if _, err := pool.Exec(ctx, "UPDATE core.app_user SET email = NULL WHERE username = 'no.mail'"); err != nil {
		t.Fatalf("clear email: %v", err)
	}

	loginFrom(t, router, companyID, "no.mail", "dev-office")
	if n := outboxCount(t, ctx, pool, "new_device_login"); n != 0 {
		t.Fatalf("user without email must not get an alert row, got %d", n)
	}
}

type recordingSender struct {
	err  error
	sent []mail.Message
}

func (s *recordingSender) Send(_ context.Context, m mail.Message) error {
	s.sent = append(s.sent, m)
	return s.err
}

func TestEmailOutbox_WorkerDelivers(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	id := enqueueTestEmail(t, ctx, sqlcgen.New(pool), "andi@example.com")
	cfg := testConfig()
	cfg.MailBatchSize, cfg.MailMaxAttempts = 20, 5

	sender := &recordingSender{}
	n, err := mail.RunOnce(ctx, pool, sender, cfg)
	if err != nil || n != 1 {
		t.Fatalf("RunOnce = %d, %v", n, err)
	}
	if len(sender.sent) != 1 || sender.sent[0].To != "andi@example.com" || sender.sent[0].HTML != "<p>html</p>" {
		t.Fatalf("unexpected sends: %+v", sender.sent)
	}
	if status, _, _, _ := outboxRow(t, ctx, pool, id); status != "sent" {
		t.Fatalf("expected sent, got %s", status)
	}
}

func TestEmailOutbox_WorkerRetriesThenGivesUp(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	id := enqueueTestEmail(t, ctx, sqlcgen.New(pool), "andi@example.com")
	cfg := testConfig()
	cfg.MailBatchSize, cfg.MailMaxAttempts = 20, 2
	sender := &recordingSender{err: errors.New("smtp dial: connection refused")}

	before := time.Now()
	if _, err := mail.RunOnce(ctx, pool, sender, cfg); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	status, attempts, next, lastErr := outboxRow(t, ctx, pool, id)
	if status != "pending" || attempts != 1 || lastErr == nil || *lastErr != "smtp dial: connection refused" {
		t.Fatalf("after first failure expected pending/1/error, got %s/%d/%v", status, attempts, lastErr)
	}
	if d := next.Sub(before); d < 2*time.Minute-5*time.Second || d > 2*time.Minute+5*time.Second {
		t.Fatalf("first retry expected ~2 minutes ahead, got %v", d)
	}

	// Make it due again and fail the last allowed attempt.
	if _, err := pool.Exec(ctx, "UPDATE core.email_outbox SET next_attempt_at = now() WHERE id = $1", id); err != nil {
		t.Fatalf("make due: %v", err)
	}
	if _, err := mail.RunOnce(ctx, pool, sender, cfg); err != nil {
		t.Fatalf("RunOnce: %v", err)
	}
	if status, attempts, _, _ := outboxRow(t, ctx, pool, id); status != "failed" || attempts != 2 {
		t.Fatalf("after MailMaxAttempts expected failed/2, got %s/%d", status, attempts)
	}
}

// TestEmailOutbox_DB_StaleSendingCountsAttempt — spec
// 2026-10-06-email-outbox-claim §2.2: an expired 'sending' row is a failed
// attempt; at the limit it is given up instead of re-sent.
func TestEmailOutbox_DB_StaleSendingCountsAttempt(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	q := sqlcgen.New(pool)
	retry := enqueueTestEmail(t, ctx, q, "retry@example.com")
	giveUp := enqueueTestEmail(t, ctx, q, "giveup@example.com")
	busy := enqueueTestEmail(t, ctx, q, "busy@example.com")
	due := enqueueTestEmail(t, ctx, q, "due@example.com")
	for _, s := range []struct {
		id       pgtype.UUID
		attempts int
		lock     string
	}{{retry, 0, "-1 minute"}, {giveUp, 4, "-1 minute"}, {busy, 0, "1 minute"}} {
		if _, err := pool.Exec(ctx,
			"UPDATE core.email_outbox SET status = 'sending', attempts = $2, locked_until = now() + $3::interval WHERE id = $1",
			s.id, s.attempts, s.lock); err != nil {
			t.Fatalf("seed sending row: %v", err)
		}
	}

	tx := beginAsRuntime(t, ctx, pool)
	claimed, err := mail.ClaimEmails(ctx, tx, 10, 120, 5)
	if err != nil {
		t.Fatalf("claim: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	got := map[pgtype.UUID]int32{}
	for _, e := range claimed {
		got[e.ID] = e.Attempts
	}
	if len(got) != 2 || got[retry] != 1 || got[due] != 0 {
		t.Fatalf("claimed = %+v, want retry (attempts 1) and due (attempts 0) only", claimed)
	}
	if _, ok := got[due]; !ok {
		t.Fatalf("due pending row must be claimed, got %+v", claimed)
	}

	if status, attempts, _, lastErr := outboxRow(t, ctx, pool, retry); status != "sending" || attempts != 1 ||
		lastErr == nil || *lastErr != "worker stopped during send (lock expired)" {
		t.Fatalf("retry row = %s/%d/%v, want sending/1/lock-expired error", status, attempts, lastErr)
	}
	if status, attempts, _, _ := outboxRow(t, ctx, pool, giveUp); status != "failed" || attempts != 5 {
		t.Fatalf("give-up row = %s/%d, want failed/5", status, attempts)
	}
	var lockedUntil *time.Time
	if err := pool.QueryRow(ctx, "SELECT locked_until FROM core.email_outbox WHERE id = $1", giveUp).Scan(&lockedUntil); err != nil {
		t.Fatalf("read locked_until: %v", err)
	}
	if lockedUntil != nil {
		t.Fatalf("given-up row must have locked_until NULL, got %v", lockedUntil)
	}
	if status, attempts, _, lastErr := outboxRow(t, ctx, pool, busy); status != "sending" || attempts != 0 || lastErr != nil {
		t.Fatalf("busy row = %s/%d/%v, want untouched sending/0/nil", status, attempts, lastErr)
	}
	if status, attempts, _, _ := outboxRow(t, ctx, pool, due); status != "sending" || attempts != 0 {
		t.Fatalf("due row = %s/%d, want sending/0", status, attempts)
	}
}

// TestEmailOutbox_DB_ClaimMigrationDownUp — 000061 down restores the
// two-parameter function (with its grant); up brings the three-parameter one back.
func TestEmailOutbox_DB_ClaimMigrationDownUp(t *testing.T) {
	ctx := context.Background()
	pool, m := roleMatrixPool(t, ctx)
	if err := m.Migrate(61); err != nil {
		t.Fatalf("migrate to 000061: %v", err)
	}
	claimArgs := func() (two, three bool) {
		t.Helper()
		if err := pool.QueryRow(ctx, `
            SELECT
              EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                       WHERE n.nspname = 'core' AND p.proname = 'claim_emails' AND p.pronargs = 2),
              EXISTS (SELECT 1 FROM pg_proc p JOIN pg_namespace n ON n.oid = p.pronamespace
                       WHERE n.nspname = 'core' AND p.proname = 'claim_emails' AND p.pronargs = 3)`).Scan(&two, &three); err != nil {
			t.Fatalf("inspect claim_emails: %v", err)
		}
		return two, three
	}
	canExec := func(signature string) bool {
		t.Helper()
		var ok bool
		if err := pool.QueryRow(ctx, "SELECT has_function_privilege('app_runtime', $1, 'EXECUTE')", signature).Scan(&ok); err != nil {
			t.Fatalf("check privilege %s: %v", signature, err)
		}
		return ok
	}
	if two, three := claimArgs(); two || !three || !canExec("core.claim_emails(integer, integer, integer)") {
		t.Fatalf("after up: two=%v three=%v, want only the 3-parameter function executable by app_runtime", two, three)
	}
	if err := m.Steps(-1); err != nil {
		t.Fatalf("down 000061: %v", err)
	}
	if two, three := claimArgs(); !two || three || !canExec("core.claim_emails(integer, integer)") {
		t.Fatalf("after down: two=%v three=%v, want only the 2-parameter function executable by app_runtime", two, three)
	}
	if err := m.Steps(1); err != nil {
		t.Fatalf("up 000061 again: %v", err)
	}
	if two, three := claimArgs(); two || !three {
		t.Fatalf("after re-up: two=%v three=%v, want only the 3-parameter function", two, three)
	}
}
