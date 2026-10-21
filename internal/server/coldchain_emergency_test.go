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

	"github.com/yogisaka/nexqia-api/internal/auth"
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
	if n != 1 {
		t.Fatalf("vvm_log rows = %d, want 1 (the rejected check must stay logged)", n)
	}

	// GET vvm-logs (amandemen 2026-10-21) returns the rejected check, newest
	// first; the lot filter matches the container's lot, a foreign lot none.
	roleID = ccOwnerRoleID(t, ctx, check, w)
	lotFilter := pgtype.UUID{}
	if err := check.QueryRow(ctx, `SELECT lot_id FROM inventory.opened_container WHERE id = $1`, container).Scan(&lotFilter); err != nil {
		t.Fatalf("read container lot: %v", err)
	}
	if err := check.Rollback(ctx); err != nil {
		t.Fatalf("rollback check tx: %v", err)
	}

	list := func(lotID *pgtype.UUID) ([]map[string]any, *httptest.ResponseRecorder, pgx.Tx) {
		txL := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
		path := "/api/v1/merchants/" + w.merchantStr + "/vvm-logs"
		if lotID != nil {
			path += "?lot_id=" + lotID.String()
		}
		cL, recL := ccContext(t, txL, w, roleID, http.MethodGet, path,
			gin.Params{{Key: "id", Value: w.merchantStr}}, nil)
		ListVvmLogsHandler(cL)
		var out struct {
			Data []map[string]any `json:"data"`
		}
		if err := json.Unmarshal(recL.Body.Bytes(), &out); err != nil {
			t.Fatalf("decode vvm-logs list: %v (%s)", err, recL.Body.String())
		}
		return out.Data, recL, txL
	}

	data, rec, txL := list(nil)
	if rec.Code != http.StatusOK || len(data) != 1 || data[0]["stage"] != float64(3) {
		t.Fatalf("vvm-logs = %d %v, want 200 with the single stage-3 log", rec.Code, data)
	}
	if data[0]["opened_container_id"] != container.String() {
		t.Fatalf("vvm-logs container = %v, want %s", data[0]["opened_container_id"], container)
	}
	_ = txL.Rollback(ctx)

	data, rec, txL = list(&lotFilter)
	if rec.Code != http.StatusOK || len(data) != 1 {
		t.Fatalf("vvm-logs by lot = %d %v, want the container's log", rec.Code, data)
	}
	_ = txL.Rollback(ctx)

	other := pgtype.UUID{Valid: true, Bytes: [16]byte{1, 2, 3, 4, 5, 6, 7, 8, 9, 10, 11, 12, 13, 14, 15, 16}}
	data, rec, txL = list(&other)
	if rec.Code != http.StatusOK || len(data) != 0 {
		t.Fatalf("vvm-logs foreign lot = %d %v, want empty", rec.Code, data)
	}
	_ = txL.Rollback(ctx)
}

// TestOpenedContainerListNames — the container list carries item_name and
// batch_no from the item/lot join (amandemen 2026-10-21, FE panel BUD).
func TestOpenedContainerListNames(t *testing.T) {
	ctx := context.Background()
	w := seedStockWorld(t, ctx, "ccnames", "081234574011", "Klinik Nama")

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	vax := ccSeedVaccineItem(t, ctx, seed, w, "NAMA") // name: 'Vaksin Uji NAMA'
	world := *w
	world.item = vax
	lot := stockSeedLot(t, ctx, seed, &world, "NAMA-1", nil, false)
	ccSeedContainer(t, ctx, seed, &world, vax, lot, time.Now().UTC().Add(24*time.Hour), 10_000)
	roleID := ccOwnerRoleID(t, ctx, seed, w)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	tx := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	c, rec := ccContext(t, tx, w, roleID, http.MethodGet,
		"/api/v1/merchants/"+w.merchantStr+"/opened-containers",
		gin.Params{{Key: "id", Value: w.merchantStr}}, nil)
	ListOpenedContainersHandler(c)
	if rec.Code != http.StatusOK {
		t.Fatalf("container list expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	var out struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("decode container list: %v", err)
	}
	_ = tx.Rollback(ctx)
	if len(out.Data) != 1 {
		t.Fatalf("container list = %d rows, want 1", len(out.Data))
	}
	row := out.Data[0]
	if row["item_name"] != "Vaksin Uji NAMA" {
		t.Fatalf("item_name = %v, want Vaksin Uji NAMA", row["item_name"])
	}
	if row["batch_no"] != "NAMA-1" {
		t.Fatalf("batch_no = %v, want NAMA-1", row["batch_no"])
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

// ---- HTTP wiring tests (NewRouter; spec §7.2 #4, §7.3) ----

// ccTestJWTSecret mirrors stockTestConfig's JWTSecret for token minting
// (testJWTSecret lives in the external server_test package).
const ccTestJWTSecret = "test-secret"

// ccHTTPWorld adds the live router and an owner access token on top of the
// stock world so the emergency/cold-chain routes can be exercised end-to-end.
type ccHTTPWorld struct {
	w      *stockWorld
	router interface {
		ServeHTTP(http.ResponseWriter, *http.Request)
	}
	token string
}

// ccSeedHTTPWorld builds the world through the real register endpoints and
// seeds one warehouse location (committed, pool connection free).
func ccSeedHTTPWorld(t *testing.T, ctx context.Context, username, phone, merchantName string) *ccHTTPWorld {
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
		VALUES ($1,$2,'room','GUD-EK','Gudang Troli', ARRAY['warehouse']::text[]) RETURNING id`,
		companyStr, merchantStr).Scan(&locationID); err != nil {
		t.Fatalf("insert location: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'ITM-EK','Obat Uji Troli','drug','generic',$2,'93100091','keras') RETURNING id`,
		companyStr, tablet).Scan(&itemID); err != nil {
		t.Fatalf("insert item: %v", err)
	}
	w := &stockWorld{
		pool: pool, company: stockMustUUID(t, ctx, tx, companyStr),
		merchant: stockMustUUID(t, ctx, tx, merchantStr), user: stockMustUUID(t, ctx, tx, userStr),
		companyStr: companyStr, merchantStr: merchantStr, userStr: userStr,
		location: locationID, item: itemID,
		day: time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}
	token, err := auth.GenerateToken(ccTestJWTSecret, userStr, companyStr, merchantStr, "", username, "cc-http-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}
	return &ccHTTPWorld{w: w, router: router, token: token}
}

// ccHTTPReq performs one authenticated request against the live router.
func ccHTTPReq(t *testing.T, h *ccHTTPWorld, method, path string, body any) *httptest.ResponseRecorder {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+h.token)
	req.Header.Set("X-Company-ID", h.w.companyStr)
	req.Header.Set("X-Merchant-ID", h.w.merchantStr)
	req.Header.Set("X-Device-Id", "cc-http-device")
	rec := httptest.NewRecorder()
	h.router.ServeHTTP(rec, req)
	return rec
}

// ccDecodeData extracts the envelope's data object.
func ccDecodeData(t *testing.T, rec *httptest.ResponseRecorder) map[string]any {
	t.Helper()
	var resp struct {
		Data map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatalf("decode response %s: %v", rec.Body.String(), err)
	}
	return resp.Data
}

// ccSeedKitLot inserts a lot for an arbitrary item (stockSeedLot is pinned to
// the world item) and returns its id.
func ccSeedKitLot(t *testing.T, ctx context.Context, tx pgx.Tx, w *stockWorld, itemID pgtype.UUID, batch string, expiry *time.Time) pgtype.UUID {
	t.Helper()
	var id pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.lot (company_id, item_id, batch_no, expiry_date)
		VALUES ($1,$2,$3,$4) RETURNING id`,
		w.companyStr, itemID, batch, expiry).Scan(&id); err != nil {
		t.Fatalf("insert kit lot %s: %v", batch, err)
	}
	return id
}

// TestEmergencyKitSealLifecycle — full §7.2 #4 loop over the real routes:
// create kit (2 standard items) → seal 201 → second seal 409 kit already
// sealed → break with one line (saldo decremented, seal broken_at stamped,
// move ref_type emergency_kit).
func TestEmergencyKitSealLifecycle(t *testing.T) {
	ctx := context.Background()
	h := ccSeedHTTPWorld(t, ctx, "ekseal", "081234574006", "Klinik Segel")
	w := h.w

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	lot1 := stockSeedLot(t, ctx, seed, w, "EK-1", nil, false)
	ccSeedBalance(t, ctx, seed, w, w.item, lot1, 10_000)
	var item2 pgtype.UUID
	if err := seed.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'ITM-EK2','Obat Uji Troli 2','drug','generic',
			(SELECT id FROM inventory.unit WHERE code='tablet'),'93100092','keras') RETURNING id`,
		w.companyStr).Scan(&item2); err != nil {
		t.Fatalf("insert item2: %v", err)
	}
	lot2 := ccSeedKitLot(t, ctx, seed, w, item2, "EK-2", nil)
	ccSeedBalance(t, ctx, seed, w, item2, lot2, 10_000)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	kitPath := "/api/v1/merchants/" + w.merchantStr + "/emergency-kits"
	rec := ccHTTPReq(t, h, http.MethodPost, kitPath, map[string]any{
		"name": "Troli IGD", "location_id": w.location.String(),
		"items": []map[string]any{
			{"item_id": w.item.String(), "base_qty": "2"},
			{"item_id": item2.String(), "base_qty": "1"},
		},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create kit expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	kitID := ccDecodeData(t, rec)["id"].(string)
	sealsPath := "/api/v1/emergency-kits/" + kitID + "/seals"

	rec = ccHTTPReq(t, h, http.MethodPost, sealsPath, map[string]any{"seal_no": "SEG-1"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seal expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	sealID := ccDecodeData(t, rec)["id"].(string)

	rec = ccHTTPReq(t, h, http.MethodPost, sealsPath, map[string]any{"seal_no": "SEG-2"})
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"kit already sealed"}` {
		t.Fatalf("second seal expected 409 kit already sealed, got %d %s", rec.Code, rec.Body.String())
	}

	breakPath := "/api/v1/emergency-kits/" + kitID + "/seals/" + sealID + "/break"
	rec = ccHTTPReq(t, h, http.MethodPost, breakPath, map[string]any{
		"reason": "kegawatdaruratan",
		"lines":  []map[string]any{{"item_id": w.item.String(), "qty": "3"}},
	})
	if rec.Code != http.StatusOK {
		t.Fatalf("break expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	data := ccDecodeData(t, rec)
	if data["broken_at"] == nil || data["broken_at"] == "" {
		t.Fatalf("broken_at must be stamped, got %v", data["broken_at"])
	}

	var bal int64
	if err := w.pool.QueryRow(ctx, `
		SELECT (quantity*1000)::bigint FROM inventory.stock_balance
		WHERE item_id=$1 AND lot_id=$2 AND status='available'`, w.item, lot1).Scan(&bal); err != nil || bal != 7_000 {
		t.Fatalf("saldo after break = %d (err %v), want 7000", bal, err)
	}
	var moves int
	if err := w.pool.QueryRow(ctx, `
		SELECT count(*) FROM inventory.stock_move
		WHERE ref_type='emergency_kit' AND ref_id=$1 AND item_id=$2 AND quantity=-3`,
		sealID, w.item).Scan(&moves); err != nil || moves != 1 {
		t.Fatalf("emergency_kit moves = %d (err %v), want 1", moves, err)
	}

	// The report no longer shows an active seal.
	rec = ccHTTPReq(t, h, http.MethodGet, "/api/v1/emergency-kits/"+kitID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("report expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	if as := ccDecodeData(t, rec)["active_seal"]; as != nil {
		t.Fatalf("active_seal after break must be absent, got %v", as)
	}
}

// TestEmergencyKitRefillIncomplete — sealing with a shortage is blocked 409
// kit refill incomplete, and passes with an override reason (201).
func TestEmergencyKitRefillIncomplete(t *testing.T) {
	ctx := context.Background()
	h := ccSeedHTTPWorld(t, ctx, "ekrefill", "081234574007", "Klinik Isi")
	w := h.w

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	lot := stockSeedLot(t, ctx, seed, w, "EK-R1", nil, false)
	ccSeedBalance(t, ctx, seed, w, w.item, lot, 2_000) // standar 5, saldo 2
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	kitPath := "/api/v1/merchants/" + w.merchantStr + "/emergency-kits"
	rec := ccHTTPReq(t, h, http.MethodPost, kitPath, map[string]any{
		"name": "Troli Isi", "location_id": w.location.String(),
		"items": []map[string]any{{"item_id": w.item.String(), "base_qty": "5"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create kit expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	kitID := ccDecodeData(t, rec)["id"].(string)

	rec = ccHTTPReq(t, h, http.MethodPost, "/api/v1/emergency-kits/"+kitID+"/seals",
		map[string]any{"seal_no": "SEG-K"})
	if rec.Code != http.StatusConflict || rec.Body.String() != `{"error":"kit refill incomplete"}` {
		t.Fatalf("short seal expected 409 kit refill incomplete, got %d %s", rec.Code, rec.Body.String())
	}
	rec = ccHTTPReq(t, h, http.MethodPost, "/api/v1/emergency-kits/"+kitID+"/seals", map[string]any{
		"seal_no": "SEG-K", "override_reason": "keputusan KIB ahli tepat",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("override seal expected 201, got %d %s", rec.Code, rec.Body.String())
	}
}

// TestEmergencyKitReport — the GET detail is the §7.2 #4 periodic check
// report: on_hand from the available saldo, nearest_expiry from stock ED and
// the active seal's age in days (0 right after sealing).
func TestEmergencyKitReport(t *testing.T) {
	ctx := context.Background()
	h := ccSeedHTTPWorld(t, ctx, "ekreport", "081234574008", "Klinik Lapor")
	w := h.w

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	ed := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC)
	lot := stockSeedLot(t, ctx, seed, w, "EK-P1", &ed, false)
	ccSeedBalance(t, ctx, seed, w, w.item, lot, 2_000)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	kitPath := "/api/v1/merchants/" + w.merchantStr + "/emergency-kits"
	rec := ccHTTPReq(t, h, http.MethodPost, kitPath, map[string]any{
		"name": "Troli Lapor", "location_id": w.location.String(),
		"items": []map[string]any{{"item_id": w.item.String(), "base_qty": "2"}},
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create kit expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	kitID := ccDecodeData(t, rec)["id"].(string)
	rec = ccHTTPReq(t, h, http.MethodPost, "/api/v1/emergency-kits/"+kitID+"/seals", map[string]any{"seal_no": "SEG-P"})
	if rec.Code != http.StatusCreated {
		t.Fatalf("seal expected 201, got %d %s", rec.Code, rec.Body.String())
	}

	rec = ccHTTPReq(t, h, http.MethodGet, "/api/v1/emergency-kits/"+kitID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("report expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	data := ccDecodeData(t, rec)
	items, ok := data["items"].([]any)
	if !ok || len(items) != 1 {
		t.Fatalf("report items = %v, want exactly one", data["items"])
	}
	item := items[0].(map[string]any)
	if item["on_hand"] != "2.000" {
		t.Fatalf("on_hand = %v, want 2.000", item["on_hand"])
	}
	if item["nearest_expiry"] != "2026-11-01" {
		t.Fatalf("nearest_expiry = %v, want 2026-11-01", item["nearest_expiry"])
	}
	seal, ok := data["active_seal"].(map[string]any)
	if !ok || seal["seal_no"] != "SEG-P" {
		t.Fatalf("active_seal = %v, want SEG-P", data["active_seal"])
	}
	// Sealed within the same transaction window: age 0 full days.
	if age, okA := seal["age_days"].(float64); !okA || age != 0 {
		t.Fatalf("age_days = %v, want 0", seal["age_days"])
	}
}

// TestColdChainRoutesHTTP — the cold chain wiring over NewRouter: an opened
// container is created 201 through the route and an out-of-range fridge
// reading is flagged in the LIST response.
func TestColdChainRoutesHTTP(t *testing.T) {
	ctx := context.Background()
	h := ccSeedHTTPWorld(t, ctx, "ekchain", "081234574009", "Klinik Rantai")
	w := h.w

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	vax := ccSeedVaccineItem(t, ctx, seed, w, "CHT")
	world := *w
	world.item = vax
	lot := stockSeedLot(t, ctx, seed, &world, "CHT-1", nil, false)
	ccSeedBalance(t, ctx, seed, &world, vax, lot, 10_000)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	// POST opened-containers → 201 (vaccine item carries BUD rules).
	rec := ccHTTPReq(t, h, http.MethodPost, "/api/v1/merchants/"+w.merchantStr+"/opened-containers", map[string]any{
		"item_id": vax.String(), "lot_id": lot.String(), "location_id": w.location.String(),
		"opened_at": "2026-10-21T08:00:00+07:00", "remaining_base_qty": "1",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("opened container expected 201, got %d %s", rec.Code, rec.Body.String())
	}

	// Fridge + out-of-range reading, flagged in the list response.
	rec = ccHTTPReq(t, h, http.MethodPost, "/api/v1/merchants/"+w.merchantStr+"/fridges", map[string]any{
		"name": "Lemari Vaksin Uji",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("create fridge expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	fridgeID := ccDecodeData(t, rec)["id"].(string)
	rec = ccHTTPReq(t, h, http.MethodPost, "/api/v1/merchants/"+w.merchantStr+"/fridge-temp-logs", map[string]any{
		"fridge_id": fridgeID, "temperature": "15.0",
	})
	if rec.Code != http.StatusCreated {
		t.Fatalf("temp log expected 201, got %d %s", rec.Code, rec.Body.String())
	}
	rec = ccHTTPReq(t, h, http.MethodGet, "/api/v1/merchants/"+w.merchantStr+"/fridge-temp-logs", nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("temp log list expected 200, got %d %s", rec.Code, rec.Body.String())
	}
	var list struct {
		Data []map[string]any `json:"data"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &list); err != nil {
		t.Fatalf("decode list: %v", err)
	}
	if len(list.Data) != 1 || list.Data[0]["out_of_range"] != true {
		t.Fatalf("list = %v, want one entry flagged out_of_range", list.Data)
	}
}

// TestVvmStageRequiredHTTP — a vaccine use from a container without a VVM
// stage is rejected 400 "vvm stage required" through the real route.
func TestVvmStageRequiredHTTP(t *testing.T) {
	ctx := context.Background()
	h := ccSeedHTTPWorld(t, ctx, "ekvvm", "081234574010", "Klinik VVMHTTP")
	w := h.w

	seed := stockRuntimeTx(t, ctx, w.pool, w.companyStr, w.merchantStr)
	vax := ccSeedVaccineItem(t, ctx, seed, w, "CVH")
	world := *w
	world.item = vax
	lot := stockSeedLot(t, ctx, seed, &world, "CVH-1", nil, false)
	ccSeedBalance(t, ctx, seed, &world, vax, lot, 10_000)
	container := ccSeedContainer(t, ctx, seed, &world, vax, lot, time.Now().UTC().Add(24*time.Hour), 10_000)
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	rec := ccHTTPReq(t, h, http.MethodPost, "/api/v1/merchants/"+w.merchantStr+"/stock/usage", map[string]any{
		"location_id": w.location.String(), "item_id": vax.String(),
		"quantity": "1", "reason": "pemakaian", "opened_container_id": container.String(),
	})
	if rec.Code != http.StatusBadRequest || rec.Body.String() != `{"error":"vvm stage required"}` {
		t.Fatalf("vaccine use without stage expected 400 vvm stage required, got %d %s", rec.Code, rec.Body.String())
	}
}
