//go:build integration

package server_test

import (
	"context"
	"errors"
	"testing"

	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5/pgconn"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// queueFlowLegacyPool starts a fresh Postgres container migrated only up to
// 000056, so a test can seed pre-#23 "legacy" data (counters/queues without a
// flow) and then run migration 000057 on top of it — the same container image
// and migrate setup as newTestPostgresPool, pinned one version earlier.
func queueFlowLegacyPool(t *testing.T, ctx context.Context) (*pgxpool.Pool, *migrate.Migrate) {
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
	if err := m.Migrate(56); err != nil {
		t.Fatalf("failed to migrate to 000056: %v", err)
	}

	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool, m
}

// TestQueueFlowDB_BackfillLegacyData — running 000057 over a database that
// already has legacy counters/queues gives the merchant one default 3-stage
// flow and fills stage_id on the legacy counter and queue rows.
func TestQueueFlowDB_BackfillLegacyData(t *testing.T) {
	ctx := context.Background()
	pool, m := queueFlowLegacyPool(t, ctx)

	// Full tenant chain (company/merchant/department/payer/person/admission/
	// queue pendaftaran) — reuse of the existing child-RLS seed helper.
	tn := seedChildRLSTenant(t, ctx, pool, "QFBF")

	// Legacy counters, one per old queue_type.
	for _, ct := range []string{"pendaftaran", "perawat", "dokter"} {
		if _, err := pool.Exec(ctx,
			"INSERT INTO operations.counter (company_id, merchant_id, queue_type, code, name) VALUES ($1, $2, $3, $4, $5)",
			tn.companyID, tn.merchantID, ct, "QF-"+ct, "Loket "+ct); err != nil {
			t.Fatalf("seed counter %s: %v", ct, err)
		}
	}
	// Legacy queue rows for the remaining queue_types.
	for _, qt := range []string{"perawat", "dokter"} {
		if _, err := pool.Exec(ctx,
			"INSERT INTO operations.queue (company_id, merchant_id, queue_type, department_id, person_id, admission_id, queue_number) VALUES ($1, $2, $3, $4, $5, $6, $7)",
			tn.companyID, tn.merchantID, qt, tn.departmentID, tn.personID, tn.admissionID, "QF-"+qt+"-001"); err != nil {
			t.Fatalf("seed queue %s: %v", qt, err)
		}
	}

	if err := m.Up(); err != nil {
		t.Fatalf("apply migration 000057: %v", err)
	}

	// Exactly one default flow for the merchant.
	var flows int
	if err := pool.QueryRow(ctx,
		"SELECT count(*) FROM operations.queue_flow WHERE merchant_id = $1 AND is_default", tn.merchantID).Scan(&flows); err != nil {
		t.Fatalf("count default flows: %v", err)
	}
	if flows != 1 {
		t.Fatalf("expected 1 default flow after backfill, got %d", flows)
	}
	var flowID string
	if err := pool.QueryRow(ctx,
		"SELECT id::text FROM operations.queue_flow WHERE merchant_id = $1 AND is_default", tn.merchantID).Scan(&flowID); err != nil {
		t.Fatalf("read default flow: %v", err)
	}

	// Three stages: pendaftaran -> perawat -> dokter, no number_prefix.
	type stageRow struct {
		seq  int
		kind string
		pref *string
	}
	var stages []stageRow
	rows, err := pool.Query(ctx,
		"SELECT seq, kind, number_prefix FROM operations.queue_stage WHERE flow_id = $1 ORDER BY seq", flowID)
	if err != nil {
		t.Fatalf("list stages: %v", err)
	}
	defer rows.Close()
	for rows.Next() {
		var s stageRow
		if err := rows.Scan(&s.seq, &s.kind, &s.pref); err != nil {
			t.Fatalf("scan stage: %v", err)
		}
		stages = append(stages, s)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate stages: %v", err)
	}
	wantStages := []stageRow{{1, "admission", nil}, {2, "nurse", nil}, {3, "physician", nil}}
	if len(stages) != len(wantStages) {
		t.Fatalf("expected 3 stages, got %d (%v)", len(stages), stages)
	}
	for i, w := range wantStages {
		if stages[i].seq != w.seq || stages[i].kind != w.kind || stages[i].pref != nil {
			t.Fatalf("stage %d: expected (%d %s no prefix), got (%d %s %v)", i, w.seq, w.kind, stages[i].seq, stages[i].kind, stages[i].pref)
		}
	}

	// Legacy counters got their stage per the old queue_type mapping.
	rows, err = pool.Query(ctx, `
		SELECT c.queue_type, st.kind
		FROM operations.counter c JOIN operations.queue_stage st ON st.id = c.stage_id
		WHERE c.merchant_id = $1 ORDER BY c.queue_type`, tn.merchantID)
	if err != nil {
		t.Fatalf("join counter stages: %v", err)
	}
	defer rows.Close()
	got := map[string]string{}
	for rows.Next() {
		var qt, kind string
		if err := rows.Scan(&qt, &kind); err != nil {
			t.Fatalf("scan counter stage: %v", err)
		}
		got[qt] = kind
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate counter stages: %v", err)
	}
	wantMap := map[string]string{"pendaftaran": "admission", "perawat": "nurse", "dokter": "physician"}
	for qt, kind := range wantMap {
		if got[qt] != kind {
			t.Fatalf("counter %s: expected stage kind %s, got %q", qt, kind, got[qt])
		}
	}
	if len(got) != 3 {
		t.Fatalf("expected 3 counters with stage_id, got %d", len(got))
	}

	// Legacy queue rows got their stage too.
	var staged int
	if err := pool.QueryRow(ctx, `
		SELECT count(*) FROM operations.queue q
		JOIN operations.queue_stage st ON st.id = q.stage_id
		WHERE q.merchant_id = $1`, tn.merchantID).Scan(&staged); err != nil {
		t.Fatalf("count staged queues: %v", err)
	}
	if staged != 3 {
		t.Fatalf("expected 3 legacy queue rows with stage_id, got %d", staged)
	}
}

// TestQueueFlowDB_Constraints — one default flow per merchant (partial unique
// index) and the number_prefix format check.
func TestQueueFlowDB_Constraints(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "qflow.cons", "081234570131")
	tx := locationTx(t, ctx, pool, companyID, merchantID)
	defer tx.Rollback(ctx)

	var flowID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.queue_flow (company_id, merchant_id, name, is_default)
		VALUES ($1, $2, 'Alur Utama', true) RETURNING id::text`,
		companyID, merchantID).Scan(&flowID); err != nil {
		t.Fatalf("insert default flow: %v", err)
	}

	// A second default flow for the same merchant → 23505 (uq_queue_flow_default).
	if _, err := tx.Exec(ctx, "SAVEPOINT sp"); err != nil {
		t.Fatalf("savepoint: %v", err)
	}
	_, err := tx.Exec(ctx, `
		INSERT INTO operations.queue_flow (company_id, merchant_id, name, is_default)
		VALUES ($1, $2, 'Alur Kedua', true)`, companyID, merchantID)
	var pgErr *pgconn.PgError
	if !errors.As(err, &pgErr) || pgErr.Code != "23505" {
		t.Fatalf("second default flow: expected 23505, got %v", err)
	}
	if _, err := tx.Exec(ctx, "ROLLBACK TO SAVEPOINT sp"); err != nil {
		t.Fatalf("rollback savepoint: %v", err)
	}

	// Invalid number_prefix (lowercase + too long) → 23514.
	locationExpectError(t, ctx, tx, "invalid prefix", "23514",
		"INSERT INTO operations.queue_stage (company_id, merchant_id, flow_id, seq, name, kind, number_prefix, served_by_permission) VALUES ($1, $2, $3, 1, 'Antri', 'admission', 'bpjs-lama', 'operations.counter.manage')",
		companyID, merchantID, flowID)

	// Valid prefix (uppercase, 1-4 chars) inserts fine.
	if _, err := tx.Exec(ctx, `
		INSERT INTO operations.queue_stage (company_id, merchant_id, flow_id, seq, name, kind, number_prefix, served_by_permission)
		VALUES ($1, $2, $3, 1, 'Antri', 'admission', 'BPJ', 'operations.counter.manage')`,
		companyID, merchantID, flowID); err != nil {
		t.Fatalf("insert stage with valid prefix: %v", err)
	}
}

// TestQueueFlowDB_RLS — the four new tables are invisible across merchants:
// as app_runtime scoped to merchant A, merchant B's flow/stage/journey/board
// rows are not visible.
func TestQueueFlowDB_RLS(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)

	tnA := seedChildRLSTenant(t, ctx, pool, "QFA")
	tnB := seedChildRLSTenant(t, ctx, pool, "QFB")

	seed := func(tn childRLSTenant, label string) {
		t.Helper()
		var flowID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO operations.queue_flow (company_id, merchant_id, name, is_default)
			VALUES ($1, $2, $3, true) RETURNING id::text`,
			tn.companyID, tn.merchantID, "Alur "+label).Scan(&flowID); err != nil {
			t.Fatalf("seed flow %s: %v", tn.merchantID, err)
		}
		var stageID string
		if err := pool.QueryRow(ctx, `
			INSERT INTO operations.queue_stage (company_id, merchant_id, flow_id, seq, name, kind, served_by_permission)
			VALUES ($1, $2, $3, 1, 'Pendaftaran', 'admission', 'operations.counter.manage') RETURNING id::text`,
			tn.companyID, tn.merchantID, flowID).Scan(&stageID); err != nil {
			t.Fatalf("seed stage %s: %v", tn.merchantID, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO operations.queue_journey (company_id, merchant_id, admission_id, person_id, flow_id, current_stage_id)
			VALUES ($1, $2, $3, $4, $5, $6)`,
			tn.companyID, tn.merchantID, tn.admissionID, tn.personID, flowID, stageID); err != nil {
			t.Fatalf("seed journey %s: %v", tn.merchantID, err)
		}
		if _, err := pool.Exec(ctx, `
			INSERT INTO operations.display_board (company_id, merchant_id, name, stage_ids, counter_ids)
			VALUES ($1, $2, $3, $4::uuid[], '{}'::uuid[])`,
			tn.companyID, tn.merchantID, "Layar "+label, "{"+stageID+"}"); err != nil {
			t.Fatalf("seed display board %s: %v", tn.merchantID, err)
		}
	}
	seed(tnA, "A")
	seed(tnB, "B")

	for _, tn := range []childRLSTenant{tnA, tnB} {
		tx := locationTx(t, ctx, pool, tn.companyID, tn.merchantID)
		for _, table := range []string{"queue_flow", "queue_stage", "queue_journey", "display_board"} {
			var own, other int
			if err := tx.QueryRow(ctx,
				"SELECT count(*) FROM operations."+table+" WHERE merchant_id = $1", tn.merchantID).Scan(&own); err != nil {
				t.Fatalf("count own %s: %v", table, err)
			}
			if err := tx.QueryRow(ctx,
				"SELECT count(*) FROM operations."+table+" WHERE merchant_id <> $1", tn.merchantID).Scan(&other); err != nil {
				t.Fatalf("count other %s: %v", table, err)
			}
			if own != 1 || other != 0 {
				t.Fatalf("%s as merchant %s: expected own=1 other=0, got own=%d other=%d", table, tn.merchantID, own, other)
			}
		}
		if err := tx.Rollback(ctx); err != nil {
			t.Fatalf("rollback: %v", err)
		}
	}
}
