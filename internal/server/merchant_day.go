// internal/server/merchant_day.go
// Merchant-local "today" and the shared daily sequence lock — spec
// 2026-10-06-daily-numbering-integrity §3.1-§3.2.
package server

import (
	"context"
	"time"

	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// defaultMerchantTimezone is core.merchant.timezone's column default
// (000003_core_tenancy.up.sql), used when the merchant row can't be read.
const defaultMerchantTimezone = "Asia/Jakarta"

// merchantTZ resolves the merchant's IANA timezone name: session dates,
// daily sequences and "today" widgets follow core.merchant.timezone. An
// invalid/unknown merchant or a read error falls back to the default.
func merchantTZ(ctx context.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) string {
	if !merchantID.Valid {
		return defaultMerchantTimezone
	}
	tz, err := q.GetMerchantTimezone(ctx, merchantID)
	if err != nil || tz == "" {
		return defaultMerchantTimezone
	}
	return tz
}

// merchantLocation loads the merchant timezone as *time.Location; it only
// fails when the stored zone name is not a valid IANA name.
func merchantLocation(ctx context.Context, q *sqlcgen.Queries, merchantID pgtype.UUID) (*time.Location, error) {
	return time.LoadLocation(merchantTZ(ctx, q, merchantID))
}

// localDayStart is midnight, in loc, of the day now falls on in loc.
func localDayStart(now time.Time, loc *time.Location) time.Time {
	y, m, d := now.In(loc).Date()
	return time.Date(y, m, d, 0, 0, 0, 0, loc)
}

// lockDailySequence takes the transaction-scoped advisory lock for one daily
// sequence ("<scope>:<local date>" per merchant). Call it right before the
// count that derives the next number; it is released at commit/rollback.
func lockDailySequence(ctx context.Context, q *sqlcgen.Queries, merchantID pgtype.UUID, scope string, dayStart time.Time) error {
	return q.LockSequence(ctx, sqlcgen.LockSequenceParams{
		Column1: merchantID.String(),
		Column2: scope + ":" + dayStart.Format("2006-01-02"),
	})
}

// txDayStart is the merchant-local midnight of the current transaction's
// start (now(), what admission_at / queue.created_at defaults record): a
// request that crosses midnight numbers its row on the day the row is
// stamped with. Use it for sequences whose rows take the DB default time.
func txDayStart(ctx context.Context, q *sqlcgen.Queries, loc *time.Location) (time.Time, error) {
	ts, err := q.TransactionTime(ctx)
	if err != nil {
		return time.Time{}, err
	}
	return localDayStart(ts.Time, loc), nil
}
