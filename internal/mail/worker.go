package mail

import (
	"context"
	"fmt"
	"log/slog"
	"strings"
	"time"

	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

const (
	claimLockSeconds = 120
	purgeInterval    = time.Hour
	maxRetryDelay    = 60 * time.Minute
)

// outboxStore is the slice of core.email_outbox the worker needs; pgStore is the
// real one, tests swap in a fake.
type outboxStore interface {
	Claim(ctx context.Context, batch int32) ([]ClaimedEmail, error)
	MarkSent(ctx context.Context, id pgtype.UUID) error
	MarkFailed(ctx context.Context, id pgtype.UUID, errText string, retryAt time.Time, maxAttempts int32) error
}

type pgStore struct {
	pool        *pgxpool.Pool
	maxAttempts int32 // MAIL_MAX_ATTEMPTS: re-claimed stale rows count toward it
}

// Claim commits the claim in its own short transaction before anything is sent,
// so a slow SMTP session never holds row locks.
func (s pgStore) Claim(ctx context.Context, batch int32) ([]ClaimedEmail, error) {
	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return nil, err
	}
	defer tx.Rollback(ctx)
	rows, err := ClaimEmails(ctx, tx, batch, claimLockSeconds, s.maxAttempts)
	if err != nil {
		return nil, err
	}
	return rows, tx.Commit(ctx)
}

func (s pgStore) MarkSent(ctx context.Context, id pgtype.UUID) error {
	return sqlcgen.New(s.pool).MarkEmailSent(ctx, id)
}

func (s pgStore) MarkFailed(ctx context.Context, id pgtype.UUID, errText string, retryAt time.Time, maxAttempts int32) error {
	return sqlcgen.New(s.pool).MarkEmailFailed(ctx, sqlcgen.MarkEmailFailedParams{
		ID: id, Error: errText, RetryAt: pgtype.Timestamptz{Time: retryAt, Valid: true}, MaxAttempts: maxAttempts,
	})
}

// RunWorker delivers due outbox rows every MailPollIntervalSeconds and purges
// old sent/failed rows once an hour, until ctx is done.
func RunWorker(ctx context.Context, pool *pgxpool.Pool, sender Sender, cfg config.Config) {
	ticker := time.NewTicker(time.Duration(cfg.MailPollIntervalSeconds) * time.Second)
	defer ticker.Stop()
	var lastPurge time.Time
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
		}
		if time.Since(lastPurge) >= purgeInterval {
			purgeOld(ctx, pool, cfg)
			lastPurge = time.Now()
		}
		if _, err := RunOnce(ctx, pool, sender, cfg); err != nil && ctx.Err() == nil {
			slog.Warn("email worker: claim failed", "error", err)
		}
	}
}

// RunOnce claims one batch and tries to send each row; it returns how many rows
// were claimed.
func RunOnce(ctx context.Context, pool *pgxpool.Pool, sender Sender, cfg config.Config) (int, error) {
	return processBatch(ctx, pgStore{pool: pool, maxAttempts: int32(cfg.MailMaxAttempts)}, sender, cfg, time.Now)
}

func processBatch(ctx context.Context, store outboxStore, sender Sender, cfg config.Config, now func() time.Time) (int, error) {
	rows, err := store.Claim(ctx, int32(cfg.MailBatchSize))
	if err != nil {
		return 0, err
	}
	for _, e := range rows {
		sendErr := safeSend(ctx, sender, Message{To: e.To, Subject: e.Subject, Text: e.Text, HTML: e.HTML})
		if sendErr == nil {
			if err := store.MarkSent(ctx, e.ID); err != nil {
				slog.Warn("email worker: mark sent failed", "id", e.ID.String(), "error", err)
			}
			continue
		}
		// SMTP replies may echo the recipient; the log only gets the masked form.
		slog.Warn("email send failed", "id", e.ID.String(), "kind", e.Kind, "to", maskAddress(e.To),
			"attempt", e.Attempts+1, "error", strings.ReplaceAll(sendErr.Error(), e.To, maskAddress(e.To)))
		if err := store.MarkFailed(ctx, e.ID, sendErr.Error(), now().Add(retryDelay(e.Attempts)), int32(cfg.MailMaxAttempts)); err != nil {
			slog.Warn("email worker: mark failed failed", "id", e.ID.String(), "error", err)
		}
	}
	return len(rows), nil
}

// retryDelay is 2^(attempts+1) minutes, capped at an hour: 2, 4, 8, 16, 32, 60, …
func retryDelay(attempts int32) time.Duration {
	if attempts >= 5 {
		return maxRetryDelay
	}
	return min(time.Duration(1<<(attempts+1))*time.Minute, maxRetryDelay)
}

// safeSend turns a panicking sender into an ordinary failed attempt so one bad
// row cannot kill the worker goroutine.
func safeSend(ctx context.Context, sender Sender, m Message) (err error) {
	defer func() {
		if r := recover(); r != nil {
			err = fmt.Errorf("send panicked: %v", r)
		}
	}()
	return sender.Send(ctx, m)
}

func purgeOld(ctx context.Context, pool *pgxpool.Pool, cfg config.Config) {
	cutoff := time.Now().AddDate(0, 0, -cfg.MailOutboxRetentionDays)
	deleted, err := sqlcgen.New(pool).PurgeEmails(ctx, pgtype.Timestamptz{Time: cutoff, Valid: true})
	if err != nil {
		if ctx.Err() == nil {
			slog.Warn("email worker: purge failed", "error", err)
		}
		return
	}
	if deleted > 0 {
		slog.Info("email worker: purged old outbox rows", "deleted", deleted)
	}
}
