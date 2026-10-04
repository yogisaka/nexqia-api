//go:build integration

package server_test

import (
	"context"
	"regexp"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// Migration 000059 consolidates role 'FO Rajal' into 'Pendaftaran' via three
// paths: (1) rename when no live 'Pendaftaran' exists in the company, (2) aside
// the soft-deleted 'Pendaftaran' blocking the UNIQUE(company_id, name) slot
// before renaming, (3) soft-delete FO Rajal and merge assignments/sessions when
// a live 'Pendaftaran' exists. All 000059-originated rows carry the
// updated_by sentinel zero-UUID marker; target roles of a merge are NOT marked.

const (
	// roleConsolidationMarker is the sentinel updated_by value written by 000059.
	roleConsolidationMarker = "00000000-0000-0000-0000-000000000000"
	// roleConsolidationSeeder is the deleted_by we use when seeding soft-deleted rows.
	roleConsolidationSeeder = "00000000-0000-0000-0000-0000000000dd"

	permPerson    = "00000000-0000-0000-0004-000000000017" // core.person.manage
	permVisit     = "00000000-0000-0000-0004-000000000023" // operations.visit.manage
	permSchedule  = "00000000-0000-0000-0004-000000000021" // operations.schedule.manage
	permDashboard = "00000000-0000-0000-0004-000000000009" // reporting.dashboard.view
	permCounter   = "00000000-0000-0000-0004-000000000022" // operations.counter.manage
)

const (
	companyA = "00000000-0000-0000-0059-000000000001" // rename path (no conflict)
	companyB = "00000000-0000-0000-0059-000000000002" // merge path (live Pendaftaran)
	companyC = "00000000-0000-0000-0059-000000000003" // aside path (soft-deleted Pendaftaran)
	companyD = "00000000-0000-0000-0059-000000000004" // control (Admin only)

	merchantA = "00000000-0000-0000-0059-000000000011"
	merchantB = "00000000-0000-0000-0059-000000000012"

	roleFOA       = "00000000-0000-0000-0059-0000000000a1"
	roleFOB       = "00000000-0000-0000-0059-0000000000b1"
	rolePendaftB  = "00000000-0000-0000-0059-0000000000b2"
	roleFOC       = "00000000-0000-0000-0059-0000000000c1"
	rolePendaftC  = "00000000-0000-0000-0059-0000000000c2"
	roleAdminD    = "00000000-0000-0000-0059-0000000000d1"
	userA         = "00000000-0000-0000-0059-0000000000aa"
	userB         = "00000000-0000-0000-0059-0000000000bb"
	umrA          = "00000000-0000-0000-0059-0000000001a1"
	umrB          = "00000000-0000-0000-0059-0000000001b1"
	ucrB          = "00000000-0000-0000-0059-0000000002b1"
	refreshTokenB = "00000000-0000-0000-0059-0000000003b1"
)

// roleConsolidationPool starts a fresh Postgres container migrated up to 000058,
// so a test can seed pre-000059 legacy role data and then run migration 000059
// on top of it — the same container image and migrate setup as
// queueFlowLegacyPool, pinned one version earlier.
func roleConsolidationPool(t *testing.T, ctx context.Context) (*pgxpool.Pool, *migrate.Migrate) {
	t.Helper()
	container, err := postgres.Run(ctx, "nexqia-api-postgres-test:latest",
		postgres.WithDatabase("nexqia_test"),
		postgres.WithUsername("nexqia"),
		postgres.WithPassword("nexqia"),
		testcontainers.WithWaitStrategy(wait.ForListeningPort("5432/tcp")),
	)
	if err != nil {
		t.Fatalf("failed to start postgres container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })

	dsn, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("failed to get connection string: %v", err)
	}
	m, err := migrate.New("file://../../migrations", dsn)
	if err != nil {
		t.Fatalf("failed to init migrate: %v", err)
	}
	if err := m.Migrate(58); err != nil {
		t.Fatalf("failed to migrate to 000058: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, m
}

func roleConsolidationExec(t *testing.T, ctx context.Context, pool *pgxpool.Pool, sql string, args ...any) {
	t.Helper()
	if _, err := pool.Exec(ctx, sql, args...); err != nil {
		t.Fatalf("seed exec failed: %v\nsql: %s", err, sql)
	}
}

func roleConsolidationSeed(t *testing.T, ctx context.Context, pool *pgxpool.Pool) {
	t.Helper()

	// Companies + merchants (company.code UNIQUE, merchant UNIQUE(company_id, code)).
	for i, id := range []string{companyA, companyB, companyC, companyD} {
		code := []string{"RC-A", "RC-B", "RC-C", "RC-D"}[i]
		roleConsolidationExec(t, ctx, pool,
			`INSERT INTO core.company (id, code, name) VALUES ($1, $2, 'RS Konsolidasi '||$2)`, id, code)
	}
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.merchant (id, company_id, code, name) VALUES
		 ($1, $2, 'M1', 'RS Konsolidasi A utama'),
		 ($3, $4, 'M1', 'RS Konsolidasi B utama')`,
		merchantA, companyA, merchantB, companyB)

	// Users (password_hash NOT NULL; username UNIQUE per company).
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.app_user (id, company_id, username, password_hash) VALUES
		 ($1, $2, 'konsol-a', 'x-hash'),
		 ($3, $4, 'konsol-b', 'x-hash')`,
		userA, companyA, userB, companyB)

	// Roles. UNIQUE(company_id, name); soft-delete CHECK requires deleted_at+deleted_by together.
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.role (id, company_id, name, description, is_system) VALUES
		 ($1, $2, 'FO Rajal', 'Front Office Rawat Jalan - pendaftaran, check-in, dan antrian', true),
		 ($3, $4, 'FO Rajal', 'Front Office Rawat Jalan - pendaftaran, check-in, dan antrian', true),
		 ($5, $6, 'Pendaftaran', 'Pendaftaran pasien (seed lama, company merge)', true),
		 ($7, $8, 'FO Rajal', 'Front Office Rawat Jalan - pendaftaran, check-in, dan antrian', true),
		 ($9, $10, 'Pendaftaran', 'Pendaftaran pasien (seed terhapus, company aside)', true),
		 ($11, $12, 'Admin', 'Administrator sistem', true)`,
		roleFOA, companyA,
		roleFOB, companyB,
		rolePendaftB, companyB,
		roleFOC, companyC,
		rolePendaftC, companyC,
		roleAdminD, companyD)

	// Company C: soft-delete its 'Pendaftaran' (blocks jalur-2 aside before 000059).
	roleConsolidationExec(t, ctx, pool,
		`UPDATE core.role SET deleted_at = now(), deleted_by = $1 WHERE id = $2`,
		roleConsolidationSeeder, rolePendaftC)

	// Company A grants: FO Rajal A gets 4 old grants (person, visit, schedule, dashboard).
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.role_permission (role_id, permission_id) VALUES
		 ($1, $2), ($1, $3), ($1, $4), ($1, $5)`,
		roleFOA, permPerson, permVisit, permSchedule, permDashboard)

	// Company B template grants: live Pendaftaran B gets 2 grants (person, visit).
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.role_permission (role_id, permission_id) VALUES ($1, $2), ($1, $3)`,
		rolePendaftB, permPerson, permVisit)

	// Assignments: A has 1 user_merchant_role -> FO Rajal; B has user_merchant_role
	// and user_company_role -> FO Rajal (merge path must move both).
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.user_merchant_role (id, user_id, merchant_id, role_id) VALUES
		 ($1, $2, $3, $4), ($5, $6, $7, $8)`,
		umrA, userA, merchantA, roleFOA,
		umrB, userB, merchantB, roleFOB)
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.user_company_role (id, user_id, company_id, role_id) VALUES ($1, $2, $3, $4)`,
		ucrB, userB, companyB, roleFOB)

	// Session with active role FO Rajal in the merge company (3c must null it).
	roleConsolidationExec(t, ctx, pool,
		`INSERT INTO core.refresh_token
		 (id, user_id, company_id, merchant_id, device_id, device_label, token_hash, expires_at, created_ip, active_role_id)
		 VALUES ($1, $2, $3, $4, 'dev-1', 'Test Device', 'hash-konsolidasi-b', now() + interval '7 days', '127.0.0.1', $5)`,
		refreshTokenB, userB, companyB, merchantB, roleFOB)
}

// TestRoleConsolidation — running 000059 over a database with all three legacy
// shapes (rename / merge / aside) plus a control company consolidates every
// 'FO Rajal' into 'Pendaftaran' and completes the 5-permission grant set.
func TestRoleConsolidation(t *testing.T) {
	ctx := context.Background()
	pool, m := roleConsolidationPool(t, ctx)
	roleConsolidationSeed(t, ctx, pool)

	if err := m.Migrate(59); err != nil {
		t.Fatalf("failed to migrate to 000059: %v", err)
	}

	t.Run("company A rename path", func(t *testing.T) {
		// Same role id, now named 'Pendaftaran', live and marked.
		var name string
		var deletedAtNull any
		err := pool.QueryRow(ctx,
			`SELECT name, deleted_at FROM core.role WHERE id = $1`, roleFOA).
			Scan(&name, &deletedAtNull)
		if err != nil {
			t.Fatalf("role A not found: %v", err)
		}
		if name != "Pendaftaran" || deletedAtNull != nil {
			t.Fatalf("A: want live 'Pendaftaran', got name=%q deleted_at=%v", name, deletedAtNull)
		}

		var updatedBy *string
		if err := pool.QueryRow(ctx,
			`SELECT updated_by::text FROM core.role WHERE id = $1`, roleFOA).Scan(&updatedBy); err != nil {
			t.Fatal(err)
		}
		if updatedBy == nil || *updatedBy != roleConsolidationMarker {
			t.Fatalf("A: want updated_by marker %s, got %v", roleConsolidationMarker, updatedBy)
		}

		// A: 4 grant lama (person, visit, schedule, dashboard) + 1 grant baru
		// (counter, DO NOTHING pada person/visit/schedule/dashboard) = TEPAT 5.
		var grants int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM core.role_permission WHERE role_id = $1`, roleFOA).Scan(&grants); err != nil {
			t.Fatal(err)
		}
		if grants != 5 {
			t.Fatalf("A: want exactly 5 grants, got %d", grants)
		}
	})

	t.Run("company B merge path", func(t *testing.T) {
		// Assignments moved: new rows point at live Pendaftaran B, the old
		// FO-Rajal-pointing rows are gone (jalur 3a/3b re-insert with new uuid,
		// then delete the FO Rajal rows).
		var umrNew, umrOld, ucrNew, ucrOld int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE role_id = $1), count(*) FILTER (WHERE role_id = $2)
			 FROM core.user_merchant_role WHERE user_id = $3`,
			rolePendaftB, roleFOB, userB).Scan(&umrNew, &umrOld); err != nil {
			t.Fatal(err)
		}
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FILTER (WHERE role_id = $1), count(*) FILTER (WHERE role_id = $2)
			 FROM core.user_company_role WHERE user_id = $3`,
			rolePendaftB, roleFOB, userB).Scan(&ucrNew, &ucrOld); err != nil {
			t.Fatal(err)
		}
		if umrNew != 1 || umrOld != 0 || ucrNew != 1 || ucrOld != 0 {
			t.Fatalf("B: assignments not moved (umr new=%d old=%d, ucr new=%d old=%d)",
				umrNew, umrOld, ucrNew, ucrOld)
		}

		// Session neutralized: active_role_id NULL.
		var activeRole *string
		if err := pool.QueryRow(ctx,
			`SELECT active_role_id::text FROM core.refresh_token WHERE id = $1`, refreshTokenB).
			Scan(&activeRole); err != nil {
			t.Fatal(err)
		}
		if activeRole != nil {
			t.Fatalf("B: want refresh_token active_role_id NULL, got %s", *activeRole)
		}

		// FO Rajal B soft-deleted (deleted_at + deleted_by both set) and marked.
		var deletedBy, updatedBy *string
		if err := pool.QueryRow(ctx,
			`SELECT deleted_by::text, updated_by::text FROM core.role WHERE id = $1`, roleFOB).
			Scan(&deletedBy, &updatedBy); err != nil {
			t.Fatal(err)
		}
		if deletedBy == nil {
			t.Fatal("B: FO Rajal should be soft-deleted (deleted_by NULL)")
		}
		if updatedBy == nil || *updatedBy != roleConsolidationMarker {
			t.Fatalf("B: FO Rajal want updated_by marker, got %v", updatedBy)
		}

		// Pendaftaran B live, NOT marked, with 5 grants
		// (2 grant template: person + visit; + 3 grant baru: counter, schedule, dashboard = 5).
		var updatedByB *string
		if err := pool.QueryRow(ctx,
			`SELECT updated_by::text FROM core.role WHERE id = $1 AND deleted_at IS NULL`, rolePendaftB).
			Scan(&updatedByB); err != nil {
			t.Fatalf("B: live Pendaftaran not found: %v", err)
		}
		if updatedByB != nil && *updatedByB == roleConsolidationMarker {
			t.Fatal("B: merge target Pendaftaran must NOT carry the marker")
		}
		var grantsB int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM core.role_permission WHERE role_id = $1`, rolePendaftB).Scan(&grantsB); err != nil {
			t.Fatal(err)
		}
		if grantsB != 5 {
			t.Fatalf("B: want 5 grants on Pendaftaran, got %d", grantsB)
		}
	})

	t.Run("company C aside path", func(t *testing.T) {
		// Old soft-deleted row renamed aside: 'Pendaftaran (terhapus YYYYMMDD)'.
		var asideName string
		if err := pool.QueryRow(ctx,
			`SELECT name FROM core.role WHERE id = $1 AND deleted_at IS NOT NULL`, rolePendaftC).
			Scan(&asideName); err != nil {
			t.Fatalf("C: soft-deleted Pendaftaran not found: %v", err)
		}
		matched := regexp.MustCompile(` \(terhapus \d+\)$`).MatchString(asideName)
		if !matched {
			t.Fatalf("C: want name ~ ' \\(terhapus \\d+\\)$', got %q", asideName)
		}

		// FO Rajal C id is now live 'Pendaftaran', marked.
		var name string
		var updatedBy *string
		var deletedAtNull any
		if err := pool.QueryRow(ctx,
			`SELECT name, updated_by::text, deleted_at FROM core.role WHERE id = $1`, roleFOC).
			Scan(&name, &updatedBy, &deletedAtNull); err != nil {
			t.Fatalf("C: role not found: %v", err)
		}
		if name != "Pendaftaran" || deletedAtNull != nil {
			t.Fatalf("C: want live 'Pendaftaran', got name=%q deleted_at=%v", name, deletedAtNull)
		}
		if updatedBy == nil || *updatedBy != roleConsolidationMarker {
			t.Fatalf("C: want updated_by marker, got %v", updatedBy)
		}
	})

	t.Run("company D control", func(t *testing.T) {
		// Admin untouched, no Pendaftaran created, no grants added.
		var roles int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM core.role WHERE company_id = $1`, companyD).Scan(&roles); err != nil {
			t.Fatal(err)
		}
		if roles != 1 {
			t.Fatalf("D: want exactly 1 role (Admin), got %d", roles)
		}
		var name string
		if err := pool.QueryRow(ctx,
			`SELECT name FROM core.role WHERE id = $1`, roleAdminD).Scan(&name); err != nil {
			t.Fatal(err)
		}
		if name != "Admin" {
			t.Fatalf("D: want Admin untouched, got %q", name)
		}
		var grantsD int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM core.role_permission WHERE role_id = $1`, roleAdminD).Scan(&grantsD); err != nil {
			t.Fatal(err)
		}
		if grantsD != 0 {
			t.Fatalf("D: want 0 grants, got %d", grantsD)
		}
	})

	t.Run("global invariant", func(t *testing.T) {
		// No live 'FO Rajal' anywhere after consolidation.
		var liveFO int
		if err := pool.QueryRow(ctx,
			`SELECT count(*) FROM core.role WHERE name = 'FO Rajal' AND deleted_at IS NULL`).
			Scan(&liveFO); err != nil {
			t.Fatal(err)
		}
		if liveFO != 0 {
			t.Fatalf("global: want 0 live 'FO Rajal', got %d", liveFO)
		}
	})
}
