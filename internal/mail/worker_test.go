package mail

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/config"
)

type fakeSender struct {
	err   error
	panic bool
	sent  []Message
}

func (f *fakeSender) Send(_ context.Context, m Message) error {
	if f.panic {
		panic("boom")
	}
	f.sent = append(f.sent, m)
	return f.err
}

type failedCall struct {
	id          pgtype.UUID
	errText     string
	retryAt     time.Time
	maxAttempts int32
}

type fakeStore struct {
	rows   []ClaimedEmail
	batch  int32
	sent   []pgtype.UUID
	failed []failedCall
}

func (f *fakeStore) Claim(_ context.Context, batch int32) ([]ClaimedEmail, error) {
	f.batch = batch
	return f.rows, nil
}

func (f *fakeStore) MarkSent(_ context.Context, id pgtype.UUID) error {
	f.sent = append(f.sent, id)
	return nil
}

func (f *fakeStore) MarkFailed(_ context.Context, id pgtype.UUID, errText string, retryAt time.Time, maxAttempts int32) error {
	f.failed = append(f.failed, failedCall{id, errText, retryAt, maxAttempts})
	return nil
}

func testUUID(b byte) pgtype.UUID {
	return pgtype.UUID{Bytes: [16]byte{b}, Valid: true}
}

var workerCfg = config.Config{MailBatchSize: 20, MailMaxAttempts: 5}

func TestProcessBatch_SuccessMarksSent(t *testing.T) {
	store := &fakeStore{rows: []ClaimedEmail{{ID: testUUID(1), To: "andi@gmail.com", Subject: "S", Text: "t", HTML: "h"}}}
	sender := &fakeSender{}
	n, err := processBatch(context.Background(), store, sender, workerCfg, time.Now)
	if err != nil || n != 1 {
		t.Fatalf("processBatch = %d, %v", n, err)
	}
	if store.batch != 20 {
		t.Errorf("claim batch = %d, want MailBatchSize 20", store.batch)
	}
	if len(sender.sent) != 1 || sender.sent[0].To != "andi@gmail.com" || sender.sent[0].Subject != "S" {
		t.Fatalf("unexpected sends: %+v", sender.sent)
	}
	if len(store.sent) != 1 || store.sent[0] != testUUID(1) || len(store.failed) != 0 {
		t.Fatalf("expected one MarkSent, got sent=%v failed=%v", store.sent, store.failed)
	}
}

func TestProcessBatch_FailureSchedulesRetry(t *testing.T) {
	now := time.Date(2026, 10, 1, 9, 0, 0, 0, time.UTC)
	store := &fakeStore{rows: []ClaimedEmail{{ID: testUUID(2), To: "andi@gmail.com", Attempts: 0}}}
	sender := &fakeSender{err: errors.New("smtp rcpt: 450 try later")}
	if _, err := processBatch(context.Background(), store, sender, workerCfg, func() time.Time { return now }); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(store.sent) != 0 || len(store.failed) != 1 {
		t.Fatalf("expected one MarkFailed, got sent=%v failed=%v", store.sent, store.failed)
	}
	f := store.failed[0]
	if f.id != testUUID(2) || f.errText != "smtp rcpt: 450 try later" || f.maxAttempts != 5 {
		t.Errorf("unexpected MarkFailed call: %+v", f)
	}
	if want := now.Add(2 * time.Minute); !f.retryAt.Equal(want) {
		t.Errorf("first retry at %v, want %v", f.retryAt, want)
	}
}

func TestProcessBatch_PanicCountsAsFailure(t *testing.T) {
	store := &fakeStore{rows: []ClaimedEmail{{ID: testUUID(3), To: "a@b.id"}, {ID: testUUID(4), To: "c@d.id"}}}
	if _, err := processBatch(context.Background(), store, &fakeSender{panic: true}, workerCfg, time.Now); err != nil {
		t.Fatalf("processBatch: %v", err)
	}
	if len(store.failed) != 2 {
		t.Fatalf("both panicking sends must be recorded as failures, got %d", len(store.failed))
	}
}

func TestRetryDelay(t *testing.T) {
	for attempts, want := range map[int32]time.Duration{
		0: 2 * time.Minute, 1: 4 * time.Minute, 2: 8 * time.Minute, 3: 16 * time.Minute,
		4: 32 * time.Minute, 5: 60 * time.Minute, 30: 60 * time.Minute,
	} {
		if got := retryDelay(attempts); got != want {
			t.Errorf("retryDelay(%d) = %v, want %v", attempts, got, want)
		}
	}
}
