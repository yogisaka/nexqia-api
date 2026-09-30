//go:build integration

package server_test

import (
	"context"
	"errors"
	"testing"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// runtimeTx opens a transaction acting as app_runtime for companyID, so RLS and
// the app_runtime grants apply (the test pool itself is the table owner).
func runtimeTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID string) pgx.Tx {
	t.Helper()
	tx, err := pool.Begin(ctx)
	if err != nil {
		t.Fatalf("begin tx: %v", err)
	}
	t.Cleanup(func() { _ = tx.Rollback(ctx) })
	if _, err := tx.Exec(ctx, "SET LOCAL ROLE app_runtime"); err != nil {
		t.Fatalf("set local role: %v", err)
	}
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_company_id', $1, true)", companyID); err != nil {
		t.Fatalf("set company GUC: %v", err)
	}
	return tx
}

// TestRoleTemplate_SeedComplete — migration 000051 seeds the three platform role
// templates with the expected roles, permission counts and physician flags.
func TestRoleTemplate_SeedComplete(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	want := map[string]struct{ roles, perms int }{
		"klinik_pratama": {5, 18},
		"klinik_utama":   {6, 20},
		"rumah_sakit":    {7, 22},
	}
	for code, w := range want {
		var version, roles, perms int
		err := pool.QueryRow(ctx, `
			SELECT t.version, count(DISTINCT tr.id), count(trp.permission_id)
			FROM core.template t
			JOIN core.template_role tr ON tr.template_id = t.id
			LEFT JOIN core.template_role_permission trp ON trp.template_role_id = tr.id
			WHERE t.code = $1 AND t.scope = 'platform' AND t.kind = 'role'
			GROUP BY t.version`, code).Scan(&version, &roles, &perms)
		if err != nil {
			t.Fatalf("%s: query template: %v", code, err)
		}
		if version != 1 || roles != w.roles || perms != w.perms {
			t.Fatalf("%s: expected version 1, %d roles, %d perms; got %d, %d, %d", code, w.roles, w.perms, version, roles, perms)
		}
	}

	var platformCount int
	if err := pool.QueryRow(ctx, "SELECT count(*) FROM core.template WHERE scope = 'platform' AND kind = 'role'").Scan(&platformCount); err != nil {
		t.Fatalf("count platform templates: %v", err)
	}
	if platformCount != 3 {
		t.Fatalf("expected 3 platform role templates, got %d", platformCount)
	}

	rows, err := pool.Query(ctx, `
		SELECT t.code, tr.name, tr.requires_physician_data
		FROM core.template_role tr JOIN core.template t ON t.id = tr.template_id
		WHERE t.scope = 'platform' AND t.kind = 'role'`)
	if err != nil {
		t.Fatalf("query template roles: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var code, name string
		var physician bool
		if err := rows.Scan(&code, &name, &physician); err != nil {
			t.Fatalf("scan template role: %v", err)
		}
		wantPhysician := name == "Dokter" || name == "Dokter Spesialis"
		if physician != wantPhysician {
			t.Fatalf("%s/%s: requires_physician_data = %v, want %v", code, name, physician, wantPhysician)
		}
		if name == "Admin RS" && code != "rumah_sakit" {
			t.Fatalf("Admin RS found in template %s", code)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate template roles: %v", err)
	}
	var adminRS int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM core.template_role tr JOIN core.template t ON t.id = tr.template_id
		WHERE tr.name = 'Admin RS' AND t.code = 'rumah_sakit'`).Scan(&adminRS); err != nil {
		t.Fatalf("count Admin RS: %v", err)
	}
	if adminRS != 1 {
		t.Fatalf("expected Admin RS once in rumah_sakit, got %d", adminRS)
	}
}

// TestRoleTemplate_RuntimeCannotWriteTemplates — app_runtime can read platform
// templates but cannot write template tables or rewrite apply history.
func TestRoleTemplate_RuntimeCannotWriteTemplates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.runtime", "081234570901")

	tx := runtimeTx(t, ctx, pool, companyID)

	stmts := []struct {
		name string
		sql  string
		args []any
	}{
		{"insert template", "INSERT INTO core.template (kind, code, name, scope, company_id, usage) VALUES ('role', 'mine', 'Mine', 'company', $1, 'copy')", []any{companyID}},
		{"update template", "UPDATE core.template SET name = 'x'", nil},
		{"delete template_role", "DELETE FROM core.template_role", nil},
		{"insert template_role_permission", "INSERT INTO core.template_role_permission (template_role_id, permission_id) SELECT template_role_id, permission_id FROM core.template_role_permission LIMIT 1", nil},
		{"update template_application", "UPDATE core.template_application SET template_version = 2", nil},
		{"delete template_application", "DELETE FROM core.template_application", nil},
	}
	for _, s := range stmts {
		if _, err := tx.Exec(ctx, "SAVEPOINT sp"); err != nil {
			t.Fatalf("savepoint: %v", err)
		}
		_, err := tx.Exec(ctx, s.sql, s.args...)
		var pgErr *pgconn.PgError
		if !errors.As(err, &pgErr) || pgErr.Code != "42501" {
			t.Fatalf("%s: expected permission denied (42501), got %v", s.name, err)
		}
		if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp"); err != nil {
			t.Fatalf("rollback to savepoint: %v", err)
		}
	}

	var visible int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.template").Scan(&visible); err != nil {
		t.Fatalf("count templates as app_runtime: %v", err)
	}
	if visible != 3 {
		t.Fatalf("expected 3 platform templates visible to app_runtime, got %d", visible)
	}
}

// TestRoleTemplate_CompanyIsolation — apply history is visible only to its own
// company under app_runtime.
func TestRoleTemplate_CompanyIsolation(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyA, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.iso.a", "081234570902")
	companyB, _, _, _, _ := seedAccountOwner(t, ctx, pool, router, "roletpl.iso.b", "081234570903")

	if _, err := pool.Exec(ctx, `
		INSERT INTO core.template_application (company_id, template_id, template_version)
		SELECT $1, id, 1 FROM core.template WHERE code = 'klinik_pratama' AND scope = 'platform'`, companyB); err != nil {
		t.Fatalf("seed template_application: %v", err)
	}

	for _, c := range []struct {
		company string
		want    int
	}{{companyA, 0}, {companyB, 1}} {
		tx := runtimeTx(t, ctx, pool, c.company)
		var n int
		if err := tx.QueryRow(ctx, "SELECT count(*) FROM core.template_application").Scan(&n); err != nil {
			t.Fatalf("count template_application: %v", err)
		}
		if n != c.want {
			t.Fatalf("company %s: expected %d template_application rows, got %d", c.company, c.want, n)
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
}
