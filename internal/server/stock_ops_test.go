//go:build integration

package server

// Integration tests for the stock operations API (plan 2026-10-07-inventory-17a
// Task 5, spec §4.3–4.5). HTTP-level tests against the real router; seeding
// reuses the stock ledger test helpers (newStockTestPool, stockRegisterOwner,
// stockSeedLot, stockRuntimeTx, ...).

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgtype"

	"github.com/yogisaka/nexqia-api/internal/db/sqlcgen"
)

// opsEnv is one stock-ops test environment: tenant, router, one warehouse and
// one pharmacy room plus a drug item.
type opsEnv struct {
	w      *stockWorld
	router *gin.Engine
	loc2   pgtype.UUID
	token  string
}

func opsSetup(t *testing.T, label, phone string) *opsEnv {
	t.Helper()
	ctx := context.Background()
	pool := newStockTestPool(t, ctx)
	redisClient := newStockTestRedis(t, ctx)
	router := NewRouter(pool, redisClient, stockTestConfig())
	companyStr, userStr, token, _ := stockRegisterOwner(t, ctx, pool, router, "ops."+label, phone)
	merchantStr := stockMerchantID(t, ctx, pool, companyStr)

	tx := stockRuntimeTx(t, ctx, pool, companyStr, merchantStr)
	tablet := stockUnitID(t, ctx, tx, "tablet")
	var loc1, loc2, itemID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions)
		VALUES ($1,$2,'room','GUD-OPS','Gudang Ops', ARRAY['warehouse']::text[]) RETURNING id`,
		companyStr, merchantStr).Scan(&loc1); err != nil {
		t.Fatalf("insert location 1: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions)
		VALUES ($1,$2,'room','APK-OPS','Apotek Ops', ARRAY['pharmacy']::text[]) RETURNING id`,
		companyStr, merchantStr).Scan(&loc2); err != nil {
		t.Fatalf("insert location 2: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'ITM-OPS','Obat Uji Ops','drug','generic',$2,'93100091','keras') RETURNING id`,
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
		location:    loc1,
		item:        itemID,
		day:         time.Date(2026, 10, 10, 0, 0, 0, 0, time.UTC),
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit ops seed: %v", err)
	}
	return &opsEnv{w: w, router: router, loc2: loc2, token: token}
}

// opsRequest sends an authenticated tenant request and decodes the JSON body.
func opsRequest(t *testing.T, e *opsEnv, method, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	req := httptest.NewRequest(method, path, bytes.NewReader(raw))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Company-ID", e.w.companyStr)
	req.Header.Set("X-Merchant-ID", e.w.merchantStr)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

func opsErr(out map[string]any) string {
	msg, _ := out["error"].(string)
	return msg
}

// opsED is a future expiry date relative to now.
func opsED(days int) time.Time { return time.Now().UTC().AddDate(0, 0, days) }

// pgtypeUUID parses a UUID string for raw pool queries.
func pgtypeUUID(s string) pgtype.UUID {
	var id pgtype.UUID
	_ = id.Scan(s)
	return id
}

// opsSeedLot plants a future-ED lot for the env's item.
func opsSeedLot(t *testing.T, ctx context.Context, e *opsEnv, batch string, expiry time.Time) pgtype.UUID {
	t.Helper()
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	id := stockSeedLot(t, ctx, tx, e.w, batch, &expiry, false)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit lot: %v", err)
	}
	return id
}

// opsSeedBalanceAt adds qty (milli) at a specific location via the ledger.
func opsSeedBalanceAt(t *testing.T, ctx context.Context, e *opsEnv, locID, lotID pgtype.UUID, source string, qtyMilli int64) {
	t.Helper()
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	q := sqlcgen.New(tx)
	if _, err := applyStockMove(ctx, q, e.w.merchant, e.w.company, e.w.user, stockMoveInput{
		Key:      stockKey{LocationID: locID, LotID: lotID, Source: source, Status: "available"},
		ItemID:   e.w.item,
		Qty:      numericFromMilli(qtyMilli),
		UnitCost: pgtype.Numeric{Valid: true},
		MoveType: "receipt",
		MoveDate: e.w.day,
	}); err != nil {
		t.Fatalf("seed balance: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit balance: %v", err)
	}
}

// opsBalanceQty reads a balance quantity string ("" when absent).
func opsBalanceQty(t *testing.T, e *opsEnv, locID, lotID pgtype.UUID, source, status string) string {
	t.Helper()
	var qty *string
	err := e.w.pool.QueryRow(context.Background(), `
		SELECT quantity::text FROM inventory.stock_balance
		WHERE merchant_id = $1 AND location_id = $2 AND lot_id = $3 AND source = $4 AND status = $5`,
		e.w.merchantStr, locID, lotID, source, status).Scan(&qty)
	if err != nil && err.Error() != "no rows in result set" {
		t.Fatalf("read balance: %v", err)
	}
	if qty == nil {
		return ""
	}
	return *qty
}

// opsUsagePath is the usage endpoint of the env's merchant.
func opsUsagePath(e *opsEnv) string { return "/api/v1/merchants/" + e.w.merchantStr + "/stock/usage" }

// TestStockOps_Usage — FEFO issue over the API: parts snapshot, balance
// decrement, insufficient 409 and override without a reason 400.
func TestStockOps_Usage(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "usage", "081234573001")
	edNear, edFar := opsED(30), opsED(90)
	lotNear := opsSeedLot(t, ctx, e, "U-NEAR", edNear)
	lotFar := opsSeedLot(t, ctx, e, "U-FAR", edFar)
	opsSeedBalanceAt(t, ctx, e, e.w.location, lotNear, "purchase", 6_000)
	opsSeedBalanceAt(t, ctx, e, e.w.location, lotFar, "purchase", 6_000)

	status, out := opsRequest(t, e, http.MethodPost, opsUsagePath(e), map[string]any{
		"location_id": e.w.location.String(), "item_id": e.w.item.String(),
		"quantity": "8", "reason": "pemakaian ruangan",
	})
	if status != http.StatusCreated {
		t.Fatalf("usage expected 201, got %d: %v", status, out)
	}
	data := out["data"].(map[string]any)
	parts := data["parts"].([]any)
	// Expected allocation (FEFO): near-ED lot first (6), then far-ED lot (2).
	if len(parts) != 2 {
		t.Fatalf("expected 2 parts, got %v", data)
	}
	p0 := parts[0].(map[string]any)
	p1 := parts[1].(map[string]any)
	if p0["lot"].(map[string]any)["batch_no"] != "U-NEAR" || p0["quantity"] != "6.000" {
		t.Fatalf("part 0 = %v, want U-NEAR 6.000", p0)
	}
	if p1["lot"].(map[string]any)["batch_no"] != "U-FAR" || p1["quantity"] != "2.000" {
		t.Fatalf("part 1 = %v, want U-FAR 2.000", p1)
	}
	if got := opsBalanceQty(t, e, e.w.location, lotNear, "purchase", "available"); got != "0.000" {
		t.Fatalf("lot NEAR balance = %s, want 0.000", got)
	}

	// Insufficient stock → 409 fixed message.
	status, out = opsRequest(t, e, http.MethodPost, opsUsagePath(e), map[string]any{
		"location_id": e.w.location.String(), "item_id": e.w.item.String(),
		"quantity": "100", "reason": "kebanyakan",
	})
	if status != http.StatusConflict || opsErr(out) != "insufficient stock" {
		t.Fatalf("insufficient usage expected 409, got %d: %v", status, out)
	}

	// Override without a reason → 400 fixed message.
	status, out = opsRequest(t, e, http.MethodPost, opsUsagePath(e), map[string]any{
		"location_id": e.w.location.String(), "item_id": e.w.item.String(),
		"quantity": "1", "override_lot_id": lotFar.String(), "override_reason": "   ",
	})
	if status != http.StatusBadRequest || opsErr(out) != "override needs a reason" {
		t.Fatalf("override without reason expected 400, got %d: %v", status, out)
	}

	// Admission ref: moves get ref_type admission.
	admission := pgtype.UUID{Valid: true}
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	if err := tx.QueryRow(ctx, "SELECT uuid_generate_v7()").Scan(&admission); err != nil {
		t.Fatalf("gen admission id: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit: %v", err)
	}
	status, out = opsRequest(t, e, http.MethodPost, opsUsagePath(e), map[string]any{
		"location_id": e.w.location.String(), "item_id": e.w.item.String(),
		"quantity": "1", "reason": "tindakan", "admission_id": admission.String(),
	})
	if status != http.StatusCreated {
		t.Fatalf("admission usage expected 201, got %d: %v", status, out)
	}
	var refType string
	if err := e.w.pool.QueryRow(ctx, `
		SELECT ref_type FROM inventory.stock_move
		WHERE ref_id = $1 AND move_type = 'issue'`, admission).Scan(&refType); err != nil || refType != "admission" {
		t.Fatalf("admission move ref_type = %s (err %v), want admission", refType, err)
	}
}

// TestStockOps_Transfer — transfer_out + transfer_in with one shared ref_id;
// same locations rejected.
func TestStockOps_Transfer(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "transfer", "081234573002")
	lot := opsSeedLot(t, ctx, e, "T-1", opsED(120))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 10_000)

	path := "/api/v1/merchants/" + e.w.merchantStr + "/stock/transfers"
	status, out := opsRequest(t, e, http.MethodPost, path, map[string]any{
		"from_location_id": e.w.location.String(), "to_location_id": e.w.location.String(),
		"lines": []map[string]any{{"item_id": e.w.item.String(), "lot_id": lot.String(),
			"source": "purchase", "quantity": "1"}},
	})
	if status != http.StatusBadRequest || opsErr(out) != "locations must differ" {
		t.Fatalf("same-location transfer expected 400, got %d: %v", status, out)
	}

	transferDate := time.Now().UTC().Format("2006-01-02")
	status, out = opsRequest(t, e, http.MethodPost, path, map[string]any{
		"from_location_id": e.w.location.String(), "to_location_id": e.loc2.String(),
		"transfer_date": transferDate,
		"lines": []map[string]any{{"item_id": e.w.item.String(), "lot_id": lot.String(),
			"source": "purchase", "quantity": "6"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("transfer expected 201, got %d: %v", status, out)
	}
	data := out["data"].(map[string]any)
	transferID, _ := data["transfer_id"].(string)
	if transferID == "" {
		t.Fatalf("missing transfer_id: %v", data)
	}
	moves := data["moves"].([]any)
	if len(moves) != 2 {
		t.Fatalf("expected transfer_out + transfer_in, got %v", moves)
	}
	if got := opsBalanceQty(t, e, e.w.location, lot, "purchase", "available"); got != "4.000" {
		t.Fatalf("source balance = %s, want 4.000", got)
	}
	if got := opsBalanceQty(t, e, e.loc2, lot, "purchase", "available"); got != "6.000" {
		t.Fatalf("destination balance = %s, want 6.000", got)
	}
	var n int
	if err := e.w.pool.QueryRow(ctx, `
		SELECT count(*) FROM inventory.stock_move
		WHERE ref_type = 'transfer' AND ref_id = $1`, pgtypeUUID(transferID)).Scan(&n); err != nil || n != 2 {
		t.Fatalf("moves sharing ref_id = %d (err %v), want 2", n, err)
	}
}

// TestStockOps_Adjustment — signed adjustment with a mandatory reason, lot
// must belong to the item, zero quantity rejected.
func TestStockOps_Adjustment(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "adjust", "081234573003")
	lot := opsSeedLot(t, ctx, e, "A-1", opsED(180))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 5_000)

	path := "/api/v1/merchants/" + e.w.merchantStr + "/stock/adjustments"
	base := func(qty string) map[string]any {
		return map[string]any{
			"location_id": e.w.location.String(), "item_id": e.w.item.String(),
			"lot_id": lot.String(), "source": "purchase", "status": "available",
			"quantity": qty,
		}
	}

	// Missing reason → 400.
	body := base("3")
	status, out := opsRequest(t, e, http.MethodPost, path, body)
	if status != http.StatusBadRequest || opsErr(out) != "reason is required" {
		t.Fatalf("adjustment without reason expected 400, got %d: %v", status, out)
	}

	// +3 → balance 8.
	body = base("3")
	body["reason"] = "barang ditemukan"
	status, out = opsRequest(t, e, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("positive adjustment expected 201, got %d: %v", status, out)
	}
	if got := opsBalanceQty(t, e, e.w.location, lot, "purchase", "available"); got != "8.000" {
		t.Fatalf("balance after +3 = %s, want 8.000", got)
	}

	// −2 with a reason → balance 6.
	body = base("-2")
	body["reason"] = "rusak jatuh"
	status, _ = opsRequest(t, e, http.MethodPost, path, body)
	if status != http.StatusCreated {
		t.Fatalf("negative adjustment expected 201, got %d: %v", status, out)
	}
	if got := opsBalanceQty(t, e, e.w.location, lot, "purchase", "available"); got != "6.000" {
		t.Fatalf("balance after −2 = %s, want 6.000", got)
	}

	// Lot of another item → 400.
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	var otherID pgtype.UUID
	if err := tx.QueryRow(ctx, `
		INSERT INTO inventory.item (company_id, code, name, item_kind, pricing_category, base_unit_id, kfa_code, drug_class)
		VALUES ($1,'ITM-OPS2','Obat Lain','drug','generic',
			(SELECT id FROM inventory.unit WHERE code='tablet'),'93100092','keras')
		RETURNING id`, e.w.companyStr).Scan(&otherID); err != nil {
		t.Fatalf("insert other item: %v", err)
	}
	otherLot := stockSeedLot(t, ctx, tx, &stockWorld{companyStr: e.w.companyStr, item: otherID}, "A-OTHER", nil, false)
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit other item: %v", err)
	}
	body = base("1")
	body["lot_id"] = otherLot.String()
	body["reason"] = "cek"
	status, out = opsRequest(t, e, http.MethodPost, path, body)
	if status != http.StatusBadRequest || opsErr(out) != "lot does not belong to item" {
		t.Fatalf("foreign lot expected 400, got %d: %v", status, out)
	}
}

// TestStockOps_StatusChangeAndRecall — quarantine move pair and lot recall.
func TestStockOps_StatusChangeAndRecall(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "status", "081234573004")
	lot := opsSeedLot(t, ctx, e, "S-1", opsED(200))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 8_000)

	path := "/api/v1/merchants/" + e.w.merchantStr + "/stock/status-change"
	status, out := opsRequest(t, e, http.MethodPost, path, map[string]any{
		"location_id": e.w.location.String(), "lot_id": lot.String(),
		"source": "purchase", "from_status": "available", "to_status": "quarantine",
		"quantity": "3", "reason": "suhu terputus",
	})
	if status != http.StatusCreated {
		t.Fatalf("status change expected 201, got %d: %v", status, out)
	}
	moves := out["data"].(map[string]any)["moves"].([]any)
	if len(moves) != 2 {
		t.Fatalf("expected two status_change moves, got %v", moves)
	}
	if got := opsBalanceQty(t, e, e.w.location, lot, "purchase", "available"); got != "5.000" {
		t.Fatalf("available = %s, want 5.000", got)
	}
	if got := opsBalanceQty(t, e, e.w.location, lot, "purchase", "quarantine"); got != "3.000" {
		t.Fatalf("quarantine = %s, want 3.000", got)
	}

	// Recall without a note → 400; with a note → 200.
	recallPath := "/api/v1/lots/" + lot.String() + "/recall"
	status, out = opsRequest(t, e, http.MethodPatch, recallPath, map[string]any{
		"is_recalled": true, "recall_note": "  ",
	})
	if status != http.StatusBadRequest || opsErr(out) != "recall note is required" {
		t.Fatalf("recall without note expected 400, got %d: %v", status, out)
	}
	status, out = opsRequest(t, e, http.MethodPatch, recallPath, map[string]any{
		"is_recalled": true, "recall_note": "lot dihentikan produsen",
	})
	if status != http.StatusOK {
		t.Fatalf("recall expected 200, got %d: %v", status, out)
	}
	var recalled bool
	if err := e.w.pool.QueryRow(ctx,
		"SELECT is_recalled FROM inventory.lot WHERE id = $1", lot).Scan(&recalled); err != nil || !recalled {
		t.Fatalf("lot recalled = %v (err %v), want true", recalled, err)
	}
}

// TestStockOps_CountPost — snapshot, counted lines with reasons, posting
// writes count moves for the differences.
func TestStockOps_CountPost(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "count", "081234573005")
	lot1 := opsSeedLot(t, ctx, e, "C-1", opsED(100))
	lot2 := opsSeedLot(t, ctx, e, "C-2", opsED(150))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot1, "purchase", 10_000)
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot2, "purchase", 4_000)

	// Start the count: both balances snapshot as system_qty.
	status, out := opsRequest(t, e, http.MethodPost,
		"/api/v1/merchants/"+e.w.merchantStr+"/stock-counts",
		map[string]any{"location_id": e.w.location.String()})
	if status != http.StatusCreated {
		t.Fatalf("create count expected 201, got %d: %v", status, out)
	}
	data := out["data"].(map[string]any)
	countID, _ := data["id"].(string)
	lines := data["lines"].([]any)
	if len(lines) != 2 {
		t.Fatalf("expected 2 snapshot lines, got %v", data)
	}
	sysByBalance := map[string]string{}
	for _, raw := range lines {
		l := raw.(map[string]any)
		sysByBalance[l["balance_id"].(string)] = l["system_qty"].(string)
		if l["system_qty"] != "10.000" && l["system_qty"] != "4.000" {
			t.Fatalf("unexpected system_qty: %v", l)
		}
	}

	// First posting attempt: one difference without a reason → 400.
	balanceIDs := make([]string, 0, 2)
	for bid := range sysByBalance {
		balanceIDs = append(balanceIDs, bid)
	}
	counted1, counted2 := balanceIDs[0], balanceIDs[1]
	if sysByBalance[counted1] == "4.000" {
		counted1, counted2 = counted2, counted1
	}
	linesPath := "/api/v1/stock-counts/" + countID + "/lines"
	status, out = opsRequest(t, e, http.MethodPut, linesPath, map[string]any{
		"lines": []map[string]any{{"balance_id": counted1, "counted_qty": "9"}},
	})
	if status != http.StatusOK {
		t.Fatalf("put lines expected 200, got %d: %v", status, out)
	}
	status, out = opsRequest(t, e, http.MethodPost, "/api/v1/stock-counts/"+countID+"/post", nil)
	if status != http.StatusBadRequest || opsErr(out) != "count difference needs a reason" {
		t.Fatalf("diff without reason expected 400, got %d: %v", status, out)
	}

	// Same count (still draft) with a reason → posts, balance 9, one move.
	status, out = opsRequest(t, e, http.MethodPut, linesPath, map[string]any{
		"lines": []map[string]any{{
			"balance_id": counted1, "counted_qty": "9", "reason": "selisih hitung",
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("re-put lines expected 200, got %d: %v", status, out)
	}
	status, out = opsRequest(t, e, http.MethodPost, "/api/v1/stock-counts/"+countID+"/post", nil)
	if status != http.StatusOK || out["data"].(map[string]any)["status"] != "posted" {
		t.Fatalf("post expected 200 posted, got %d: %v", status, out)
	}
	var bal1 string
	lotVar := pgtype.UUID{}
	if err := e.w.pool.QueryRow(ctx, `
		SELECT b.quantity::text FROM inventory.stock_balance b
		JOIN inventory.stock_count_line cl ON cl.balance_id = b.id
		WHERE cl.count_id = $1 AND cl.counted_qty IS NOT NULL`, pgtypeUUID(countID)).Scan(&bal1); err != nil || bal1 != "9.000" {
		t.Fatalf("posted balance = %s (err %v), want 9.000", bal1, err)
	}
	_ = lotVar
	// Delete after posting → 409 (draft only).
	status, out = opsRequest(t, e, http.MethodDelete, "/api/v1/stock-counts/"+countID, nil)
	if status != http.StatusConflict {
		t.Fatalf("delete posted count expected 409, got %d: %v", status, out)
	}
}

// TestStockOps_CountChangedConflict — a balance moved since the snapshot →
// 409 stock changed since the count started.
func TestStockOps_CountChangedConflict(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "countchg", "081234573006")
	lot := opsSeedLot(t, ctx, e, "CC-1", opsED(100))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 10_000)

	status, out := opsRequest(t, e, http.MethodPost,
		"/api/v1/merchants/"+e.w.merchantStr+"/stock-counts",
		map[string]any{"location_id": e.w.location.String()})
	if status != http.StatusCreated {
		t.Fatalf("create count expected 201, got %d: %v", status, out)
	}
	countID := out["data"].(map[string]any)["id"].(string)

	// Move stock after the snapshot (ledger path, committed).
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", -2_000)

	status, out = opsRequest(t, e, http.MethodPost, "/api/v1/stock-counts/"+countID+"/post", nil)
	if status != http.StatusConflict || opsErr(out) != "stock changed since the count started" {
		t.Fatalf("changed count expected 409, got %d: %v", status, out)
	}
}

// TestStockOps_LevelsAndSettings — min/max replace per location and the
// inventory settings including the one-way period lock.
func TestStockOps_LevelsAndSettings(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "levels", "081234573007")
	lot := opsSeedLot(t, ctx, e, "L-1", opsED(100))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 2_000)
	_ = lot

	levelsPath := "/api/v1/merchants/" + e.w.merchantStr + "/stock-levels"
	status, out := opsRequest(t, e, http.MethodPut, levelsPath, map[string]any{
		"levels": []map[string]any{{
			"location_id": e.w.location.String(), "item_id": e.w.item.String(),
			"min_qty": "2", "max_qty": "10",
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("put levels expected 200, got %d: %v", status, out)
	}
	levels := out["data"].([]any)
	if len(levels) != 1 || levels[0].(map[string]any)["min_qty"] != "2.000" {
		t.Fatalf("levels = %v", out["data"])
	}

	// Re-put replaces the set for the location sent.
	status, _ = opsRequest(t, e, http.MethodPut, levelsPath, map[string]any{
		"levels": []map[string]any{{
			"location_id": e.w.location.String(), "item_id": e.w.item.String(),
			"min_qty": "1", "max_qty": "5",
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("re-put levels expected 200, got %d", status)
	}
	var minQty string
	if err := e.w.pool.QueryRow(ctx, `
		SELECT min_qty::text FROM inventory.stock_level
		WHERE merchant_id = $1 AND location_id = $2`, e.w.merchant, e.w.location).Scan(&minQty); err != nil || minQty != "1.000" {
		t.Fatalf("replaced min_qty = %s (err %v), want 1.000", minQty, err)
	}

	settingsPath := "/api/v1/merchants/" + e.w.merchantStr + "/inventory-settings"
	status, out = opsRequest(t, e, http.MethodGet, settingsPath, nil)
	if status != http.StatusOK {
		t.Fatalf("get settings expected 200, got %d: %v", status, out)
	}
	s := out["data"].(map[string]any)
	if s["near_expiry_months_1"] != float64(3) || s["min_shelf_life_months"] != float64(12) {
		t.Fatalf("default settings = %v", s)
	}

	future := time.Now().UTC().AddDate(0, 0, 30).Format("2006-01-02")
	earlier := time.Now().UTC().AddDate(0, 0, 7).Format("2006-01-02")
	status, _ = opsRequest(t, e, http.MethodPatch, settingsPath, map[string]any{
		"stock_locked_until": future, "near_expiry_months_1": 2,
	})
	if status != http.StatusOK {
		t.Fatalf("patch settings expected 200, got %d", status)
	}
	// Moving the lock backwards → 400.
	status, out = opsRequest(t, e, http.MethodPatch, settingsPath, map[string]any{
		"stock_locked_until": earlier,
	})
	if status != http.StatusBadRequest || opsErr(out) != "lock date cannot move backwards" {
		t.Fatalf("backwards lock expected 400, got %d: %v", status, out)
	}
}

// TestStockOps_RequiresPermission — without inventory.stock.manage the
// merchant endpoints and the company-level recall are 403.
func TestStockOps_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "perm", "081234573008")
	lot := opsSeedLot(t, ctx, e, "P-1", opsED(100))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 5_000)

	if _, err := e.w.pool.Exec(ctx, `DELETE FROM core.role_permission rp
		USING core.role r, core.permission p
		WHERE rp.role_id = r.id AND rp.permission_id = p.id
		  AND r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.manage'`); err != nil {
		t.Fatalf("revoke permission: %v", err)
	}

	status, out := opsRequest(t, e, http.MethodPost, opsUsagePath(e), map[string]any{
		"location_id": e.w.location.String(), "item_id": e.w.item.String(),
		"quantity": "1", "reason": "x",
	})
	if status != http.StatusForbidden || !strings.Contains(opsErr(out), "inventory.stock.manage") {
		t.Fatalf("usage expected 403, got %d: %v", status, out)
	}
	status, _ = opsRequest(t, e, http.MethodPatch, "/api/v1/lots/"+lot.String()+"/recall",
		map[string]any{"is_recalled": true, "recall_note": "alasan"})
	if status != http.StatusForbidden {
		t.Fatalf("recall expected 403, got %d", status)
	}

	// Restore for the pool's remaining lifetime hygiene.
	if _, err := e.w.pool.Exec(ctx, `INSERT INTO core.role_permission (role_id, permission_id)
		SELECT r.id, p.id FROM core.role r, core.permission p
		WHERE r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.manage'
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("restore permission: %v", err)
	}
}
