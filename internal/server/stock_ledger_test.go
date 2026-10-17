//go:build integration

package server

// Integration tests for the locked stock ledger (plan 2026-10-07-inventory-17a
// Task 2, spec §4.2–4.5). The ledger functions are unexported, so this file is
// in package server; the container/tenant seeding helpers from the
// package server_test files are duplicated here in minimal form.

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/golang-migrate/migrate/v4"
	_ "github.com/golang-migrate/migrate/v4/database/postgres"
	_ "github.com/golang-migrate/migrate/v4/source/file"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/redis/go-redis/v9"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	tcredis "github.com/testcontainers/testcontainers-go/modules/redis"
	"github.com/testcontainers/testcontainers-go/wait"

	"github.com/yogisaka/nexqia-api/internal/config"
	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

func stockTestConfig() config.Config {
	return config.Config{
		JWTSecret:                      "test-secret",
		RateLimitLoginMaxAttempts:      5,
		RateLimitLoginWindowSeconds:    900,
		RateLimitRegisterMaxAttempts:   5,
		RateLimitRegisterWindowSeconds: 900,
		RateLimitAPITokensPerMinute:    100,
		RateLimitAPIBurst:              20,
		Argon2MemoryKiB:                19456,
		Argon2Iterations:               2,
		Argon2Parallelism:              1,
		PasswordHashMaxConcurrent:      4,
		PasswordHashQueueTimeoutMS:     2000,
		MFASecretEncryptionKey:         "MDEyMzQ1Njc4OTAxMjM0NTY3ODkwMTIzNDU2Nzg5MDE=",
		AccessTokenTTLMinutes:          15,
		RefreshTokenIdleTimeoutHours:   24,
		RefreshTokenAbsoluteTTLDays:    30,
		CookieSecure:                   true,
	}
}

func newStockTestRedis(t *testing.T, ctx context.Context) *redis.Client {
	container, err := tcredis.Run(ctx, "redis:8-alpine")
	if err != nil {
		t.Fatalf("failed to start redis container: %v", err)
	}
	t.Cleanup(func() { container.Terminate(ctx) })
	host, err := container.Host(ctx)
	if err != nil {
		t.Fatalf("failed to get redis host: %v", err)
	}
	port, err := container.MappedPort(ctx, "6379/tcp")
	if err != nil {
		t.Fatalf("failed to get redis port: %v", err)
	}
	client := redis.NewClient(&redis.Options{Addr: host + ":" + port.Port()})
	t.Cleanup(func() { client.Close() })
	return client
}

func newStockTestPool(t *testing.T, ctx context.Context) *pgxpool.Pool {
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
	if err := m.Up(); err != nil {
		t.Fatalf("failed to apply migrations: %v", err)
	}
	pool, err := pgxpool.New(ctx, dsn)
	if err != nil {
		t.Fatalf("failed to create pool: %v", err)
	}
	t.Cleanup(pool.Close)
	return pool
}

func stockRuntimeTx(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID, merchantID string) pgx.Tx {
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
	if _, err := tx.Exec(ctx, "SELECT set_config('app.current_merchant_id', $1, true)", merchantID); err != nil {
		t.Fatalf("set merchant GUC: %v", err)
	}
	return tx
}

func stockMustUUID(t *testing.T, ctx context.Context, tx pgx.Tx, s string) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, "SELECT $1::uuid", s).Scan(&id); err != nil {
		t.Fatalf("parse uuid %s: %v", s, err)
	}
	return id
}

func stockUnitID(t *testing.T, ctx context.Context, tx pgx.Tx, code string) string {
	t.Helper()
	var id string
	if err := tx.QueryRow(ctx, "SELECT id::text FROM inventory.unit WHERE code = $1", code).Scan(&id); err != nil {
		t.Fatalf("resolve unit %s: %v", code, err)
	}
	return id
}

type stockWorld struct {
	pool                             *pgxpool.Pool
	company, merchant, user          pgtype.UUID
	companyStr, merchantStr, userStr string
	location, item                   pgtype.UUID
	day                              time.Time
}

// seedStockWorld registers a tenant (real endpoints, same as other integration
// fixtures) and seeds one warehouse location plus one drug item. The seed
// transaction is committed so the pool connection is free for the test.
func seedStockWorld(t *testing.T, ctx context.Context, username, phone, merchantName string) *stockWorld {
	t.Helper()
	pool := newStockTestPool(t, ctx)
	redisClient := newStockTestRedis(t, ctx)
	router := NewRouter(pool, redisClient, stockTestConfig())

	companyStr, userStr, _, _ := stockRegisterOwner(t, ctx, pool, router, username, phone)
	merchantStr := stockMerchantID(t, ctx, pool, companyStr)

	tx := stockRuntimeTx(t, ctx, pool, companyStr, merchantStr)
	tablet := stockUnitID(t, ctx, tx, "tablet")
	var locationID, itemID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions)
		VALUES ($1,$2,'room','GUD-1','Gudang Utama', ARRAY['warehouse']::text[]) RETURNING id`,
		companyStr, merchantStr).Scan(&locationID); err != nil {
		t.Fatalf("insert location: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'ITM-STK','Obat Uji Stok','drug','generic',$2,'93100090','keras') RETURNING id`,
		companyStr, tablet).Scan(&itemID); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	w := &stockWorld{
		pool:        pool,
		company:     stockMustUUID(t, ctx, tx, companyStr),
		merchant:    stockMustUUID(t, ctx, tx, merchantStr),
		user:        stockMustUUID(t, ctx, tx, userStr),
		companyStr:  companyStr,
		merchantStr: merchantStr,
		userStr:     userStr,
		location:    locationID,
		item:        itemID,
		day:         time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit stock seed: %v", err)
	}
	return w
}

func stockSeedLot(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, batch string, expiry *time.Time, recalled bool) pgtype.UUID {
	t.Helper()
	note := ""
	if recalled {
		note = "recall " + batch
	}
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.lot (company_id, item_id, batch_no, expiry_date, is_recalled, recall_note)
		VALUES ($1,$2,$3,$4,$5,$6) RETURNING id`,
		w.companyStr, w.item, batch, expiry, recalled, note).Scan(&id); err != nil {
		t.Fatalf("insert lot %s: %v", batch, err)
	}
	return id
}

// stockSeedBalance adds qty (milli-units) to one balance via applyStockMove —
// the same path production receipts take.
func stockSeedBalance(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, lotID pgtype.UUID, source, status string, qtyMilli int64, receivedAt time.Time) {
	t.Helper()
	q := sqlcgen.New(tx)
	if _, err := applyStockMove(ctx, q, w.merchant, w.company, w.user, stockMoveInput{
		Key:        stockKey{LocationID: w.location, LotID: lotID, Source: source, Status: status},
		ItemID:     w.item,
		Qty:        numericFromMilli(qtyMilli),
		UnitCost:   pgtype.Numeric{Valid: true},
		MoveType:   "receipt",
		MoveDate:   w.day,
		ReceivedAt: receivedAt,
	}); err != nil {
		t.Fatalf("seed balance %s/%s: %v", source, status, err)
	}
}

func TestStockLedger_FEFOThenFIFO(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "stockfefe", "081234572001", "Klinik FEFO")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	nearED := w.day.AddDate(0, 1, 0)
	sameED := w.day.AddDate(0, 2, 0)
	farED := w.day.AddDate(0, 3, 0)
	tOld, tNew := w.day.Add(-40*24*time.Hour), w.day.Add(-20*24*time.Hour)

	lotNear := stockSeedLot(t, ctx, tx, w, "B-NEAR", &nearED, false)
	lotSameOld := stockSeedLot(t, ctx, tx, w, "B-SAME-1", &sameED, false)
	lotSameNew := stockSeedLot(t, ctx, tx, w, "B-SAME-2", &sameED, false)
	lotFar := stockSeedLot(t, ctx, tx, w, "B-FAR", &farED, false)
	stockSeedBalance(t, ctx, tx, w, lotNear, "purchase", "available", 5_000, tOld)
	stockSeedBalance(t, ctx, tx, w, lotSameOld, "purchase", "available", 5_000, tOld)
	stockSeedBalance(t, ctx, tx, w, lotSameNew, "purchase", "available", 5_000, tNew)
	stockSeedBalance(t, ctx, tx, w, lotFar, "purchase", "available", 5_000, tNew)

	// Expected pick order (spec §4.4): ED day+30 first, then the two ED-equal
	// lots by received_first_at (tOld before tNew), then ED day+90 last.
	parts, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(12_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, "")
	if err != nil {
		t.Fatalf("issueStock: %v", err)
	}
	if len(parts) != 3 {
		t.Fatalf("expected 3 issued parts, got %d: %+v", len(parts), parts)
	}
	if parts[0].LotID != lotNear || parts[0].milliQty() != 5_000 {
		t.Fatalf("part 1 must draw lot NEAR (ED day+30) 5 units, got %+v", parts[0])
	}
	if parts[1].LotID != lotSameOld || parts[1].milliQty() != 5_000 {
		t.Fatalf("part 2 must draw the earlier-received same-ED lot, got %+v", parts[1])
	}
	if parts[2].LotID != lotSameNew || parts[2].milliQty() != 2_000 {
		t.Fatalf("part 3 must draw the later-received same-ED lot 2 units, got %+v", parts[2])
	}

	var balNear int64
	if err := tx.QueryRow(ctx, `
		SELECT (quantity * 1000)::bigint FROM inventory.stock_balance
		WHERE lot_id = $1 AND status = 'available'`, lotNear).Scan(&balNear); err != nil {
		t.Fatalf("read lot NEAR balance: %v", err)
	}
	if balNear != 0 {
		t.Fatalf("lot NEAR must be exhausted, got %d milli", balNear)
	}
}

func TestStockLedger_SkipsExpiredRecalledQuarantine(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "stockskip", "081234572002", "Klinik Skip")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	expiredED := w.day.AddDate(0, 0, -1)
	okED := w.day.AddDate(0, 1, 0)
	lotExpired := stockSeedLot(t, ctx, tx, w, "B-EXP", &expiredED, false)
	lotRecalled := stockSeedLot(t, ctx, tx, w, "B-REC", &okED, true)
	lotOK := stockSeedLot(t, ctx, tx, w, "B-OK", &okED, false)
	stockSeedBalance(t, ctx, tx, w, lotExpired, "purchase", "available", 10_000, w.day)
	stockSeedBalance(t, ctx, tx, w, lotRecalled, "purchase", "available", 10_000, w.day)
	stockSeedBalance(t, ctx, tx, w, lotOK, "purchase", "available", 10_000, w.day)
	stockSeedBalance(t, ctx, tx, w, lotOK, "purchase", "quarantine", 6_000, w.day)

	parts, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(10_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, "")
	if err != nil {
		t.Fatalf("issueStock: %v", err)
	}
	if len(parts) != 1 || parts[0].LotID != lotOK {
		t.Fatalf("only the OK lot may be issued, got %+v", parts)
	}
	var quar int64
	if err := tx.QueryRow(ctx, `
		SELECT (quantity * 1000)::bigint FROM inventory.stock_balance
		WHERE lot_id = $1 AND status = 'quarantine'`, lotOK).Scan(&quar); err != nil {
		t.Fatalf("read quarantine balance: %v", err)
	}
	if quar != 6_000 {
		t.Fatalf("quarantine balance must stay untouched, got %d milli", quar)
	}

	if _, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(1_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, ""); !errors.Is(err, errInsufficientStock) {
		t.Fatalf("second issue must fail with insufficient stock, got %v", err)
	}
}

func TestStockLedger_Insufficient(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "stockinsuf", "081234572003", "Klinik Insuf")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	ed := w.day.AddDate(0, 1, 0)
	lot := stockSeedLot(t, ctx, tx, w, "B-SIX", &ed, false)
	stockSeedBalance(t, ctx, tx, w, lot, "purchase", "available", 6_000, w.day)

	var movesBefore int
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM inventory.stock_move").Scan(&movesBefore); err != nil {
		t.Fatalf("count moves: %v", err)
	}

	parts, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(10_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, "")
	if !errors.Is(err, errInsufficientStock) {
		t.Fatalf("expected errInsufficientStock, got %v (parts %+v)", err, parts)
	}
	if len(parts) != 0 {
		t.Fatalf("no partial writes allowed, got parts %+v", parts)
	}
	var movesAfter, bal int64
	if err := tx.QueryRow(ctx, "SELECT count(*) FROM inventory.stock_move").Scan(&movesAfter); err != nil {
		t.Fatalf("count moves after: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		SELECT (quantity * 1000)::bigint FROM inventory.stock_balance WHERE lot_id = $1`, lot).Scan(&bal); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if movesAfter != int64(movesBefore) || bal != 6_000 {
		t.Fatalf("insufficient issue must write nothing, moves %d→%d balance %d", movesBefore, movesAfter, bal)
	}
}

func TestStockLedger_Override(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "stockovr", "081234572004", "Klinik Override")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	nearED := w.day.AddDate(0, 0, 10)
	farED := w.day.AddDate(0, 6, 0)
	lotNear := stockSeedLot(t, ctx, tx, w, "B-NEAR", &nearED, false)
	lotFar := stockSeedLot(t, ctx, tx, w, "B-FAR", &farED, false)
	stockSeedBalance(t, ctx, tx, w, lotNear, "purchase", "available", 4_000, w.day)
	stockSeedBalance(t, ctx, tx, w, lotFar, "purchase", "available", 4_000, w.day)

	// FEFO default: the near-ED lot goes first.
	parts, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(4_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, "")
	if err != nil || len(parts) != 1 || parts[0].LotID != lotNear {
		t.Fatalf("default FEFO issue must draw lot NEAR, got %v %+v", err, parts)
	}

	// Override without a reason is rejected and writes nothing.
	if _, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(4_000), w.day, stockRef{RefType: "usage"}, lotFar, "  "); !errors.Is(err, errOverrideReason) {
		t.Fatalf("override without reason must fail, got %v", err)
	}
	var balFar int64
	if err := tx.QueryRow(ctx, `
		SELECT (quantity * 1000)::bigint FROM inventory.stock_balance WHERE lot_id = $1`, lotFar).Scan(&balFar); err != nil {
		t.Fatalf("read lot FAR balance: %v", err)
	}
	if balFar != 4_000 {
		t.Fatalf("rejected override must not write, lot FAR balance %d", balFar)
	}

	// Valid override draws the far-ED lot first.
	parts, err = issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(4_000), w.day, stockRef{RefType: "usage"}, lotFar, "permintaan dokter batch khusus")
	if err != nil || len(parts) != 1 || parts[0].LotID != lotFar {
		t.Fatalf("override issue must draw lot FAR, got %v %+v", err, parts)
	}

	// Override of a lot with no eligible balance cannot be satisfied.
	missing := pgtype.UUID{Valid: true}
	if err := tx.QueryRow(ctx, "SELECT uuid_generate_v7()").Scan(&missing); err != nil {
		t.Fatalf("gen uuid: %v", err)
	}
	if _, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(1_000), w.day, stockRef{RefType: "usage"}, missing, "lot tidak ada"); !errors.Is(err, errInsufficientStock) {
		t.Fatalf("override of unknown lot must fail with insufficient stock, got %v", err)
	}
}

func TestStockLedger_PeriodLocked(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "stocklock", "081234572005", "Klinik Lock")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	q := sqlcgen.New(tx)

	ed := w.day.AddDate(0, 1, 0)
	lot := stockSeedLot(t, ctx, tx, w, "B-LOCK", &ed, false)
	stockSeedBalance(t, ctx, tx, w, lot, "purchase", "available", 10_000, w.day)
	if _, err := tx.Exec(ctx,
		"UPDATE inventory.merchant_setting SET stock_locked_until = $1 WHERE merchant_id = $2",
		w.day, w.merchant); err != nil {
		t.Fatalf("lock period: %v", err)
	}

	// A move dated on the locked day is rejected...
	if _, err := applyStockMove(ctx, q, w.merchant, w.company, w.user, stockMoveInput{
		Key:      stockKey{LocationID: w.location, LotID: lot, Source: "purchase", Status: "available"},
		ItemID:   w.item,
		Qty:      numericFromMilli(1_000),
		MoveType: "receipt",
		MoveDate: w.day,
	}); !errors.Is(err, errStockPeriodLocked) {
		t.Fatalf("move on locked day must fail, got %v", err)
	}
	// ...and mapped to 409 with the fixed message.
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	if !respondStockError(c, errStockPeriodLocked) {
		t.Fatal("respondStockError must handle errStockPeriodLocked")
	}
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"stock period is locked"}` {
		t.Fatalf("period-locked response = %d %s", rec.Code, rec.Body.String())
	}
	rec2 := httptest.NewRecorder()
	c2, _ := gin.CreateTestContext(rec2)
	if !respondStockError(c2, errInsufficientStock) || rec2.Code != http.StatusConflict || rec2.Body.String() != `{"error":"insufficient stock"}` {
		t.Fatalf("insufficient response = %d %s", rec2.Code, rec2.Body.String())
	}
	rec3 := httptest.NewRecorder()
	c3, _ := gin.CreateTestContext(rec3)
	if !respondStockError(c3, errOverrideReason) || rec3.Code != http.StatusBadRequest || rec3.Body.String() != `{"error":"override needs a reason"}` {
		t.Fatalf("override response = %d %s", rec3.Code, rec3.Body.String())
	}

	// Moves dated after the locked day go through.
	late := w.day.AddDate(0, 0, 1)
	parts, err := issueStock(ctx, q, w.merchant, w.company, w.user, w.location, w.item,
		numericFromMilli(2_000), late, stockRef{RefType: "usage"}, pgtype.UUID{}, "")
	if err != nil || len(parts) != 1 {
		t.Fatalf("issue after locked day must succeed, got %v %+v", err, parts)
	}
	var moveDate string
	if err := tx.QueryRow(ctx,
		"SELECT move_date::text FROM inventory.stock_move WHERE move_type = 'issue'").Scan(&moveDate); err != nil {
		t.Fatalf("read issue move: %v", err)
	}
	if moveDate != late.Format("2006-01-02") {
		t.Fatalf("issue move_date = %s, want %s", moveDate, late.Format("2006-01-02"))
	}
}

func TestStockLedger_Concurrent(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "stockconc", "081234572006", "Klinik Konkuren")
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	ed := w.day.AddDate(0, 1, 0)
	lot := stockSeedLot(t, ctx, tx, w, "B-CONC", &ed, false)
	stockSeedBalance(t, ctx, tx, w, lot, "purchase", "available", 10_000, w.day)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// Each goroutine runs its own runtime transaction issuing 8 units from a
	// balance of 10 — exactly one may succeed. No t.Fatalf inside goroutines.
	run := func() error {
		gctx := context.Background()
		gtx, err := w.pool.Begin(gctx)
		if err != nil {
			return err
		}
		defer func() { _ = gtx.Rollback(gctx) }()
		if _, err := gtx.Exec(gctx, "SET LOCAL ROLE app_runtime"); err != nil {
			return err
		}
		if _, err := gtx.Exec(gctx, "SELECT set_config('app.current_company_id', $1, true)", w.companyStr); err != nil {
			return err
		}
		if _, err := gtx.Exec(gctx, "SELECT set_config('app.current_merchant_id', $1, true)", w.merchantStr); err != nil {
			return err
		}
		gq := sqlcgen.New(gtx)
		if _, err := issueStock(gctx, gq, w.merchant, w.company, w.user, w.location, w.item,
			numericFromMilli(8_000), w.day, stockRef{RefType: "usage"}, pgtype.UUID{}, ""); err != nil {
			return err
		}
		return gtx.Commit(gctx)
	}

	var wg sync.WaitGroup
	errs := make([]error, 2)
	for i := 0; i < 2; i++ {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			errs[i] = run()
		}(i)
	}
	wg.Wait()

	successes, insufficient := 0, 0
	for _, err := range errs {
		switch {
		case err == nil:
			successes++
		case errors.Is(err, errInsufficientStock):
			insufficient++
		default:
			t.Fatalf("unexpected concurrent error: %v", err)
		}
	}
	if successes != 1 || insufficient != 1 {
		t.Fatalf("exactly one concurrent issue must succeed, got %d successes / %d insufficient (errs %v %v)",
			successes, insufficient, errs[0], errs[1])
	}

	check := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	var bal int64
	if err := check.QueryRow(ctx, `
		SELECT (quantity * 1000)::bigint FROM inventory.stock_balance WHERE lot_id = $1`, lot).Scan(&bal); err != nil {
		t.Fatalf("read balance: %v", err)
	}
	if bal != 2_000 {
		t.Fatalf("balance after concurrent issues must be 2 units, got %d milli", bal)
	}
}

// milliQty is a test-only accessor reading an issuedPart's quantity in
// thousandths (numeric(14,3) milli-units).
func (p issuedPart) milliQty() int64 {
	m, _ := numericToMilli(p.Qty)
	return m
}

// stockRegisterOwner mirrors the account_test seeding: register via the real
// endpoint, then create a merchant via the real endpoint.
func stockRegisterOwner(t *testing.T, ctx context.Context, pool *pgxpool.Pool, router *gin.Engine, username, phone string) (companyID, userID, token string, refreshCookie *http.Cookie) {
	t.Helper()
	body, _ := json.Marshal(map[string]string{
		"company_name": "Test Stock Co", "full_name": "Test Owner",
		"username": username, "email": username + "@example.com", "phone": phone,
		"password": "Passw0rd123!", "password_confirmation": "Passw0rd123!",
	})
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/register", bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("X-Device-Id", "stock-ledger-test-device")
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("register expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Data struct {
			CompanyID string `json:"company_id"`
			UserID    string `json:"user_id"`
			Token     string `json:"token"`
		} `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode register response: %v", err)
	}
	for _, ck := range rec.Result().Cookies() {
		if ck.Name == "refresh_token" {
			refreshCookie = ck
		}
	}
	if refreshCookie == nil {
		t.Fatal("expected refresh_token cookie from register")
	}

	mBody, _ := json.Marshal(map[string]string{
		"company_id": resp.Data.CompanyID, "name": "Gudang Stok", "timezone": "Asia/Jakarta",
	})
	req = httptest.NewRequest(http.MethodPost, "/api/v1/merchants", bytes.NewReader(mBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+resp.Data.Token)
	req.Header.Set("X-Company-ID", resp.Data.CompanyID)
	req.AddCookie(refreshCookie)
	rec = httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create merchant expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	return resp.Data.CompanyID, resp.Data.UserID, resp.Data.Token, refreshCookie
}

// stockMerchantID resolves the merchant id created by stockRegisterOwner.
func stockMerchantID(t *testing.T, ctx context.Context, pool *pgxpool.Pool, companyID string) string {
	t.Helper()
	var merchantID string
	if err := pool.QueryRow(ctx, "SELECT id::text FROM core.merchant WHERE company_id = $1", companyID).Scan(&merchantID); err != nil {
		t.Fatalf("resolve merchant id: %v", err)
	}
	return merchantID
}
