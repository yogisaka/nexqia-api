//go:build integration

package server

// Cold chain integration tests (spec 2026-10-20-inventory-17b-procurement-
// special-cases §7.2 #1–#3). Handler-level DIRECT invocation: the handlers
// run inside a runtime transaction with a gin test-context + httptest
// recorder, WITHOUT NewRouter/routes — HTTP status claims through the full
// router belong to the wiring task. Seeding reuses the stock ledger helpers
// (seedStockWorld, stockSeedLot, stockSeedBalance, stockRuntimeTx, ...).

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// ccContext builds a gin test context bound to the runtime tx with the auth
// keys the permission helpers read, ready to call a handler directly.
func ccContext(t *testing.T, tx pgx.Tx, w *stockWorld, roleID pgtype.UUID, method, path string, params gin.Params, body any) (*gin.Context, *httptest.ResponseRecorder) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	rec := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(rec)
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	c.Request = httptest.NewRequest(method, path, bytes.NewReader(raw))
	c.Request.Header.Set("Content-Type", "application/json")
	c.Params = params
	c.Set(txContextKey, tx)
	c.Set(authUserIDContextKey, w.user)
	c.Set(authCompanyIDContextKey, w.company)
	c.Set(authRoleIDContextKey, roleID)
	return c, rec
}

// ccFinish commits the tx after a successful (or KeepTxOnError, e.g. the VVM
// 409) response; a failing response rolls back like RunRequestTx would.
func ccFinish(t *testing.T, ctx context.Context, c *gin.Context, tx pgx.Tx, rec *httptest.ResponseRecorder) {
	t.Helper()
	if rec.Code < 400 || c.GetBool(txKeepOnErrorKey) {
		if err := tx.Commit(ctx); err != nil {
			t.Fatalf("commit handler tx: %v", err)
		}
		return
	}
	_ = tx.Rollback(ctx)
}

func ccOwnerRoleID(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		SELECT umr.role_id FROM core.user_merchant_role umr
		WHERE umr.user_id = $1 AND umr.merchant_id = $2 LIMIT 1`,
		w.user, w.merchant).Scan(&id); err != nil {
		t.Fatalf("resolve owner role: %v", err)
	}
	return id
}

// ccSeedContainer inserts an open container row directly (mirrors what
// CreateOpenedContainerHandler writes).
func ccSeedContainer(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, itemID, lotID pgtype.UUID, budAt time.Time, remainingMilli int64) pgtype.UUID {
	t.Helper()
	remaining := fmt.Sprintf("%d.%03d", remainingMilli/1000, remainingMilli%1000)
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.opened_container
			(company_id, merchant_id, item_id, lot_id, location_id, opened_at, reconstituted, bud_at, remaining_base_qty)
		VALUES ($1,$2,$3,$4,$5, now(), false, $6, $7::numeric) RETURNING id`,
		w.companyStr, w.merchantStr, itemID, lotID, w.location, budAt, remaining).Scan(&id); err != nil {
		t.Fatalf("seed opened container: %v", err)
	}
	return id
}

// ccSeedVaccineItem inserts a vaccine item with BUD rules (open 24 h,
// reconstituted 6 h) and returns its id.
func ccSeedVaccineItem(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, tag string) pgtype.UUID {
	t.Helper()
	tablet := stockUnitID(t, ctx, tx, "tablet")
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class,
			bud_after_open_hours, bud_after_reconstitution_hours)
		VALUES ($1,$2,'Vaksin Uji '||$3,'vaccine','generic',$4,'93100093','keras', 24, 6) RETURNING id`,
		w.companyStr, "ITM-"+tag, tag, tablet).Scan(&id); err != nil {
		t.Fatalf("insert vaccine item: %v", err)
	}
	return id
}

// ccSeedBalance adds qty (milli) of one item/lot at the world location.
func ccSeedBalance(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, itemID, lotID pgtype.UUID, qtyMilli int64) {
	t.Helper()
	q := sqlcgen.New(tx)
	if _, err := applyStockMove(ctx, q, w.merchant, w.company, w.user, stockMoveInput{
		Key:      stockKey{LocationID: w.location, LotID: lotID, Source: "purchase", Status: "available"},
		ItemID:   itemID,
		Qty:      numericFromMilli(qtyMilli),
		UnitCost: pgtype.Numeric{Valid: true},
		MoveType: "receipt",
		MoveDate: w.day,
	}); err != nil {
		t.Fatalf("seed balance: %v", err)
	}
}

// ccUsage calls StockUsageHandler with a usage body; extra fields ride in
// extra (merged into the JSON body).
func ccUsage(t *testing.T, ctx context.Context, w *stockWorld, roleID pgtype.UUID, qty string, extra map[string]any) (*gin.Context, *httptest.ResponseRecorder, pgx.Tx) {
	t.Helper()
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	body := map[string]any{
		"location_id": w.location.String(), "item_id": w.item.String(),
		"quantity": qty, "reason": "pemakaian",
	}
	for k, v := range extra {
		body[k] = v
	}
	c, rec := ccContext(t, tx, w, roleID, http.MethodPost,
		"/api/v1/merchants/"+w.merchantStr+"/stock/usage",
		gin.Params{{Key: "id", Value: w.merchantStr}}, body)
	StockUsageHandler(c)
	return c, rec, tx
}

func ccReadContainer(t *testing.T, ctx context.Context, w *stockWorld, id pgtype.UUID) (remaining string, status string) {
	t.Helper()
	if err := w.pool.QueryRow(ctx, `
		SELECT remaining_base_qty::text, status FROM inventory.opened_container WHERE id = $1`,
		id).Scan(&remaining, &status); err != nil {
		t.Fatalf("read container: %v", err)
	}
	return remaining, status
}

// TestOpenedContainerBUD — pure BUD computation (open vs reconstituted rule)
// and the 400 "no bud rule" mapping for items without the attributes.
func TestOpenedContainerBUD(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "ccbud", "081234574001", "Klinik BUD")

	open24, reconst6 := int32(24), int32(6)
	opened := time.Date(2026, 10, 21, 8, 0, 0, 0, time.UTC)
	bud, err := computeBudAt(opened, false, &open24, &reconst6)
	if err != nil || !bud.Equal(opened.Add(24*time.Hour)) {
		t.Fatalf("open branch bud = %v (err %v), want opened+24h", bud, err)
	}
	bud, err = computeBudAt(opened, true, &open24, &reconst6)
	if err != nil || !bud.Equal(opened.Add(6*time.Hour)) {
		t.Fatalf("reconstituted branch bud = %v (err %v), want opened+6h", bud, err)
	}
	if _, err := computeBudAt(opened, false, nil, nil); err == nil {
		t.Fatal("item without rules must fail")
	}

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	stockSeedLot(t, ctx, seed, w, "BUD-1", nil, false)
	roleID := ccOwnerRoleID(t, ctx, seed, w)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// w.item carries no BUD attributes → handler maps errNoBudRule to 400.
	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	c, rec := ccContext(t, tx, w, roleID, http.MethodPost,
		"/api/v1/merchants/"+w.merchantStr+"/opened-containers",
		gin.Params{{Key: "id", Value: w.merchantStr}}, map[string]any{
			"item_id": w.item.String(), "lot_id": w.item.String(),
			"location_id":        w.location.String(),
			"opened_at":          "2026-10-21T08:00:00+07:00",
			"remaining_base_qty": "1",
		})
	CreateOpenedContainerHandler(c)
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"no bud rule"}` {
		t.Fatalf("no-rule item expected 400 no bud rule, got %d %s", rec.Code, rec.Body.String())
	}
	_ = tx.Rollback(ctx)
}

// TestUsageFromContainer — usage with opened_container_id decrements both
// the balance and the container; zero remaining finishes the container;
// over-remaining is rejected 400 before any write.
func TestUsageFromContainer(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "ccusage", "081234574002", "Klinik Pakai")

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	lot := stockSeedLot(t, ctx, seed, w, "USE-1", nil, false)
	ccSeedBalance(t, ctx, seed, w, w.item, lot, 10_000)
	container := ccSeedContainer(t, ctx, seed, w, w.item, lot, time.Now().UTC().Add(24*time.Hour), 10_000)
	roleID := ccOwnerRoleID(t, ctx, seed, w)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// Use 4 of 10: balance 6, remaining 6.
	c, rec, tx := ccUsage(t, ctx, w, roleID, "4", map[string]any{"opened_container_id": container.String()})
	if rec.Code != http.StatusCreated {
		t.Fatalf("container usage expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	ccFinish(t, ctx, c, tx, rec)
	if got, _ := ccReadContainer(t, ctx, w, container); got != "6.000" {
		t.Fatalf("remaining after using 4 = %s, want 6.000", got)
	}
	var bal string
	if err := w.pool.QueryRow(ctx, `SELECT quantity::text FROM inventory.stock_balance WHERE lot_id = $1 AND status='available'`, lot).Scan(&bal); err != nil || bal != "6.000" {
		t.Fatalf("balance after using 4 = %s (err %v), want 6.000", bal, err)
	}

	// Exceeding the remaining qty while still open → 400 fixed message
	// (guard before write; the container still holds 6).
	c, rec, tx = ccUsage(t, ctx, w, roleID, "7", map[string]any{"opened_container_id": container.String()})
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"container qty exceeds remaining"}` {
		t.Fatalf("over-remaining usage expected 400, got %d %s", rec.Code, rec.Body.String())
	}
	_ = tx.Rollback(ctx)

	// Use the rest: remaining 0 → status finished.
	c, rec, tx = ccUsage(t, ctx, w, roleID, "6", map[string]any{"opened_container_id": container.String()})
	if rec.Code != http.StatusCreated {
		t.Fatalf("final usage expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	ccFinish(t, ctx, c, tx, rec)
	remaining, status := ccReadContainer(t, ctx, w, container)
	if remaining != "0.000" || status != "finished" {
		t.Fatalf("container = %s/%s, want 0.000/finished", remaining, status)
	}

	// A finished container is no longer open → 409 (status guard precedes
	// the quantity guard in the contract's guard order).
	_, rec, tx = ccUsage(t, ctx, w, roleID, "2", map[string]any{"opened_container_id": container.String()})
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"container not open"}` {
		t.Fatalf("finished container usage expected 409 container not open, got %d %s", rec.Code, rec.Body.String())
	}
	_ = tx.Rollback(ctx)
}

// TestUsageFromContainerPermission — a user whose active role holds ONLY
// inventory.coldchain.log may use stock from a container, but plain usage
// is rejected with EXACTLY ONE 403 (single-write guard, no route involved).
func TestUsageFromContainerPermission(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "ccperm", "081234574003", "Klinik Izin")

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	lot := stockSeedLot(t, ctx, seed, w, "PERM-1", nil, false)
	ccSeedBalance(t, ctx, seed, w, w.item, lot, 10_000)
	container := ccSeedContainer(t, ctx, seed, w, w.item, lot, time.Now().UTC().Add(24*time.Hour), 10_000)
	var ccRole pgtype.UUID
	if err := seed.QueryRow(ctx, `
		INSERT INTO core.role (company_id, name) VALUES ($1, 'Petugas Vaksin') RETURNING id`,
		w.companyStr).Scan(&ccRole); err != nil {
		t.Fatalf("insert restricted role: %v", err)
	}
	if _, err := seed.Exec(ctx, `
		INSERT INTO core.role_permission (role_id, permission_id)
		SELECT $1, p.id FROM core.permission p WHERE p.code = 'inventory.coldchain.log'`, ccRole); err != nil {
		t.Fatalf("grant coldchain.log: %v", err)
	}
	if _, err := seed.Exec(ctx, `
		INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1,$2,$3)`,
		w.user, w.merchant, ccRole); err != nil {
		t.Fatalf("assign restricted role: %v", err)
	}
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// With a container: inventory.coldchain.log alone is enough.
	c, rec, tx := ccUsage(t, ctx, w, ccRole, "4", map[string]any{"opened_container_id": container.String()})
	if rec.Code < 200 || rec.Code >= 300 {
		t.Fatalf("cold-chain usage expected 2xx, got %d %s", rec.Code, rec.Body.String())
	}
	ccFinish(t, ctx, c, tx, rec)
	if got, _ := ccReadContainer(t, ctx, w, container); got != "6.000" {
		t.Fatalf("remaining after cold-chain usage = %s, want 6.000", got)
	}

	// Without a container: stock.manage is required — exactly one 403 write.
	c, rec, tx = ccUsage(t, ctx, w, ccRole, "1", nil)
	if rec.Code != http.StatusForbidden || rec.Body.String() != `{"error":"missing permission: inventory.stock.manage"}` {
		t.Fatalf("plain usage expected single 403, got %d %s", rec.Code, rec.Body.String())
	}
	_ = tx.Rollback(ctx)
}

// TestUsageBeyondUseDate — a container past its BUD is rejected 409.
func TestUsageBeyondUseDate(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "ccbudpast", "081234574004", "Klinik Lewat")

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	lot := stockSeedLot(t, ctx, seed, w, "PAST-1", nil, false)
	ccSeedBalance(t, ctx, seed, w, w.item, lot, 10_000)
	container := ccSeedContainer(t, ctx, seed, w, w.item, lot, time.Now().UTC().Add(-1*time.Hour), 10_000)
	roleID := ccOwnerRoleID(t, ctx, seed, w)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	_, rec, tx := ccUsage(t, ctx, w, roleID, "1", map[string]any{"opened_container_id": container.String()})
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"beyond use date"}` {
		t.Fatalf("expired BUD usage expected 409 beyond use date, got %d %s", rec.Code, rec.Body.String())
	}
	_ = tx.Rollback(ctx)
}

// TestVvmRejectedButLogged — a vaccine vial at VVM stage 3 is rejected 409
// "vvm failed" AND the vvm_log row persists (KeepTxOnError).
func TestVvmRejectedButLogged(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "ccvvm", "081234574005", "Klinik VVM")

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	vax := ccSeedVaccineItem(t, ctx, seed, w, "VVM")
	world := *w
	world.item = vax
	lot := stockSeedLot(t, ctx, seed, &world, "VVM-1", nil, false)
	ccSeedBalance(t, ctx, seed, &world, vax, lot, 10_000)
	container := ccSeedContainer(t, ctx, seed, &world, vax, lot, time.Now().UTC().Add(24*time.Hour), 10_000)
	roleID := ccOwnerRoleID(t, ctx, seed, w)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	vaxWorld := world
	c, rec, tx := ccUsage(t, ctx, &vaxWorld, roleID, "1", map[string]any{
		"opened_container_id": container.String(), "vvm_stage": 3,
	})
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"vvm failed"}` {
		t.Fatalf("vvm stage 3 expected 409 vvm failed, got %d %s", rec.Code, rec.Body.String())
	}
	ccFinish(t, ctx, c, tx, rec) // KeepTxOnError: the log must survive the 409

	var n int
	check := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	if err := check.QueryRow(ctx, `
		SELECT count(*) FROM inventory.vvm_log WHERE opened_container_id = $1 AND stage = 3`,
		container).Scan(&n); err != nil {
		t.Fatalf("count vvm_log: %v", err)
	}
	_ = check.Rollback(ctx)
	if n != 1 {
		t.Fatalf("vvm_log rows = %d, want 1 (the rejected check must stay logged)", n)
	}
}

// TestFridgeTempFlag — the +2..+8 °C range check (unit, spec §7.1).
func TestFridgeTempFlag(t *testing.T) {
	if fridgeTempOutOfRange(5.5) {
		t.Fatal("5.5 must be in range (out_of_range false)")
	}
	if !fridgeTempOutOfRange(9.5) {
		t.Fatal("9.5 must be out of range")
	}
	if !fridgeTempOutOfRange(1.0) {
		t.Fatal("1.0 must be out of range")
	}
	if fridgeTempOutOfRange(8.0) || fridgeTempOutOfRange(2.0) {
		t.Fatal("2.0 and 8.0 are the inclusive limits")
	}
}
