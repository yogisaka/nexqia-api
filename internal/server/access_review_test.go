//go:build integration

package server_test

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// insertAccessReview inserts one core.access_review row through the owner pool
// (bypasses RLS). Empty reviewedBy falls back to ownerID.
func insertAccessReview(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, ownerID, notes string) {
	t.Helper()
	if _, err := pool.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, now() - interval '30 days', now(), $2, 0, $3)`,
		companyID, ownerID, notes); err != nil {
		t.Fatalf("insert access_review: %v", err)
	}
}

// TestAccessReview_AppRuntimeCannotUpdateOrDelete — as app_runtime an INSERT is
// allowed (append-only ledger) but UPDATE and DELETE are permission denied.
func TestAccessReview_AppRuntimeCannotUpdateOrDelete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.append", "081234590101")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	if _, err := tx.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, $2, $3, $4, 1, 'quarterly review ok')`,
		companyID, time.Now().Add(-30*24*time.Hour), time.Now(), userID); err != nil {
		t.Fatalf("insert as app_runtime must succeed: %v", err)
	}
	if err := tx.Rollback(ctx); err != nil {
		t.Fatalf("rollback tx: %v", err)
	}

	// Denials each get their own tx: a failed statement aborts it.
	for _, stmt := range []string{
		"UPDATE core.access_review SET notes = 'tampered'",
		"DELETE FROM core.access_review",
	} {
		tx, err := pool.Begin(ctx)
		if err != nil {
			t.Fatalf("begin tx: %v", err)
		}
		if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
			t.Fatalf("set local role: %v", err)
		}
		if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
			t.Fatalf("set company GUC: %v", err)
		}
		if _, err := tx.Exec(ctx, stmt); err == nil {
			t.Fatalf("%s as app_runtime must be denied, got no error", stmt)
		} else if !strings.Contains(err.Error(), "permission denied") {
			t.Fatalf("expected permission denied for %s, got: %v", stmt, err)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback tx: %v", err)
		}
	}
}

// TestAccessReview_RLSIsolatesCompanies — as app_runtime bound to company A,
// rows of company B stay invisible even with an explicit WHERE (RLS policy).
func TestAccessReview_RLSIsolatesCompanies(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, _, userA, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.rlsa", "081234590111")
	companyB, _, userB, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.rlsb", "081234590112")

	insertAccessReview(t, ctx, pool, companyA, userA, "company A review")
	insertAccessReview(t, ctx, pool, companyB, userB, "company B review")

	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	defer tx.Rollback(ctx)
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyA); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}

	var bVisible, aVisible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.access_review WHERE company_id = $1", companyB).Scan(&bVisible); err != nil {
		t.Fatalf("count company B rows as app_runtime: %v", err)
	}
	if bVisible != 0 {
		t.Fatalf("company B rows must be invisible under company A RLS, got %d", bVisible)
	}
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.access_review WHERE company_id = $1", companyA).Scan(&aVisible); err != nil {
		t.Fatalf("count company A rows as app_runtime: %v", err)
	}
	if aVisible != 1 {
		t.Fatalf("company A row must stay visible under company A RLS, got %d", aVisible)
	}
}

// TestAccessReview_ChecksRejectBadRows — the table CHECKs reject an inverted
// review period and an empty notes field.
func TestAccessReview_ChecksRejectBadRows(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, userID, _, _ := seedAccountOwner(t, ctx, pool, router, "accrev.checks", "081234590121")

	if _, err := pool.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, $2, $3, $4, 0, 'inverted period')`,
		companyID, time.Now(), time.Now().Add(-24*time.Hour), userID); err == nil {
		t.Fatalf("period_to <= period_from must be rejected")
	}
	if _, err := pool.Exec(ctx, `INSERT INTO core.access_review
		(company_id, period_from, period_to, reviewed_by, flagged_users, notes)
		VALUES ($1, $2, $3, $4, 0, '')`,
		companyID, time.Now().Add(-24*time.Hour), time.Now(), userID); err == nil {
		t.Fatalf("empty notes must be rejected")
	}
}
