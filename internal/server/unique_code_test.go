//go:build integration

package server_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
	"github.com/yogisaka/nexqia-api/internal/server"
)

func insertCompanyFn(ctx context.Context) func(q *sqlcgen.Queries, code string) (sqlcgen.CoreCompany, error) {
	return func(q *sqlcgen.Queries, code string) (sqlcgen.CoreCompany, error) {
		return q.CreateCompany(ctx, sqlcgen.CreateCompanyParams{Code: code, Name: "New Co", CreatedBy: pgtype.UUID{}})
	}
}

func TestInsertWithUniqueCode_RetriesAfterCollision(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (code, name, max_concurrent_sessions) VALUES ('DUPE01', 'Existing', 10)"); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	codes := []string{"DUPE01", "FRESH1"}
	calls := 0
	gen := func() (string, error) {
		code := codes[calls]
		calls++
		return code, nil
	}
	company, err := server.InsertWithUniqueCode(ctx, tx, gen, insertCompanyFn(ctx))
	if err != nil {
		t.Fatalf("expected success on 2nd attempt, got: %v", err)
	}
	if company.Code != "FRESH1" || calls != 2 {
		t.Fatalf("expected code FRESH1 after 2 calls, got %q after %d", company.Code, calls)
	}
	var one int
	if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("outer transaction must stay usable after a rolled-back attempt: %v", err)
	}
}

func TestInsertWithUniqueCode_GivesUpAfterMaxAttempts(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	if _, err := pool.Exec(ctx, "INSERT INTO core.company (code, name, max_concurrent_sessions) VALUES ('DUPE01', 'Existing', 10)"); err != nil {
		t.Fatalf("seed company: %v", err)
	}
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin: %v", err)
	}
	defer tx.Rollback(ctx)

	calls := 0
	gen := func() (string, error) {
		calls++
		return "DUPE01", nil
	}
	_, err = server.InsertWithUniqueCode(ctx, tx, gen, insertCompanyFn(ctx))
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("expected unique violation 23505 after max attempts, got: %v", err)
	}
	if calls != 5 {
		t.Fatalf("expected 5 attempts, got %d", calls)
	}
	var one int
	if err := tx.QueryRow(ctx, "SELECT 1").Scan(&one); err != nil {
		t.Fatalf("outer transaction must stay usable after giving up: %v", err)
	}
}
