//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/jackc/pgx/v5/pgxpool"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestGoodsReceipt_* covers suppliers and the goods-receipt API (plan
// 2026-10-07-inventory-17a Task 4, spec inventory §4.1): posting writes lots,
// balances and moves, the moving-average HPP is recomputed in SQL, program
// stock stays out of the cost basis, short shelf life 409s with a per-line
// report until a reason overrides it, posting a formulary item refreshes the
// reference price and regenerates proposals, and void reverses the stock and
// the average only while nothing was used.

var grKfaSeq int64 = 930002000

// grEnv is one goods-receipt test environment.
type grEnv struct {
	pool       *pgxpool.Pool
	router     *gin.Engine
	ctx        context.Context
	companyID  string
	merchantID string
	token      string
	locationID string
}

// grSetup seeds company/merchant/owner plus a warehouse room.
func grSetup(t *testing.T, label, phone string) *grEnv {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, token, _ := seedAccountOwner(t, ctx, pool, router, "gr."+label, phone)
	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantID)
	var locationID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room',$3,'Gudang Utama',ARRAY['warehouse']::text[])
		RETURNING id::text`, companyID, merchantID, "LOC-GR-"+label).Scan(&locationID); err != nil {
		t.Fatalf("insert location: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit location: %v", err)
	}
	return &grEnv{pool: pool, router: router, ctx: ctx, companyID: companyID,
		merchantID: merchantID, token: token, locationID: locationID}
}

// grRequest sends a tenant request as the owner and decodes the JSON body.
func grRequest(t *testing.T, e *grEnv, method, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rec := accountRequest(e.router, method, path, e.token, e.companyID, e.merchantID, raw)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// grCreateSupplier creates a merchant-level supplier via the API.
func grCreateSupplier(t *testing.T, e *grEnv, code string) string {
	t.Helper()
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/suppliers", map[string]any{
		"merchant_id": e.merchantID, "code": code, "name": "Supplier " + code,
	})
	if status != http.StatusCreated {
		t.Fatalf("create supplier expected 201, got %d: %v", status, out)
	}
	return out["data"].(map[string]any)["id"].(string)
}

// grItem creates a drug item (base tablet unit, factor 1) and returns
// (itemID, unitRowID).
func grItem(t *testing.T, e *grEnv, code string, extra map[string]any) (string, string) {
	t.Helper()
	grKfaSeq++
	tablet := invUnitID(t, e.ctx, e.pool, "tablet")
	base := map[string]any{"base_unit_id": tablet, "kfa_code": fmt.Sprintf("%d", grKfaSeq)}
	for k, v := range extra {
		base[k] = v
	}
	_, out, itemID := invCreateItem(t, e.router, e.token, e.companyID, e.merchantID, code, base)
	if itemID == "" {
		t.Fatalf("missing item id: %v", out)
	}
	status, outGet := invGet(t, e.router, e.token, e.companyID, e.merchantID, itemID)
	data := invDetail(t, status, outGet, http.StatusOK)
	units, _ := data["units"].([]any)
	if len(units) == 0 {
		t.Fatalf("no units: %v", data)
	}
	return itemID, units[0].(map[string]any)["id"].(string)
}

// grCreateDraft posts a receipt draft and returns (status, body, id).
func grCreateDraft(t *testing.T, e *grEnv, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/goods-receipts", body)
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id, _ = data["id"].(string)
	}
	return status, out, id
}

// grPost posts a draft and returns (status, body).
func grPost(t *testing.T, e *grEnv, id string) (int, map[string]any) {
	t.Helper()
	return grRequest(t, e, http.MethodPost, "/api/v1/goods-receipts/"+id+"/post", nil)
}

// grToday is today (offset applied) as YYYY-MM-DD.
func grToday(offsetDays int) string {
	y, m, d := time.Now().UTC().AddDate(0, 0, offsetDays).Date()
	return fmt.Sprintf("%04d-%02d-%02d", y, m, d)
}

// grDraftBody is a valid one-line purchase draft body.
func grDraftBody(e *grEnv, supplierID, itemID, unitRowID string, extra map[string]any) map[string]any {
	body := map[string]any{
		"supplier_id": supplierID, "location_id": e.locationID, "source": "purchase",
		"received_on": grToday(0),
		"lines": []map[string]any{{
			"item_id": itemID, "item_unit_id": unitRowID,
			"pack_qty": "10", "pack_price": "10000",
			// Items created through the API track batch/expiry by default,
			// so the spec requires both on every line.
			"batch_no": "B-STD", "expiry_date": grToday(700),
		}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// grBalanceQty reads the available quantity of one balance row ("" when the
// row is absent).
func grBalanceQty(t *testing.T, e *grEnv, itemID string, source string) string {
	t.Helper()
	var qty *string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT quantity::text FROM inventory.stock_balance
		WHERE merchant_id = $1 AND item_id = $2 AND source = $3 AND status = 'available'`,
		e.merchantID, itemID, source).Scan(&qty); err != nil && err.Error() != "no rows in result set" {
		t.Fatalf("read balance: %v", err)
	}
	if qty == nil {
		return ""
	}
	return *qty
}

// grAvgCost reads the item's stored average cost ("" when absent).
func grAvgCost(t *testing.T, e *grEnv, itemID string) string {
	t.Helper()
	var avg *string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT avg_cost::text FROM inventory.item_cost
		WHERE merchant_id = $1 AND item_id = $2`, e.merchantID, itemID).Scan(&avg); err != nil && err.Error() != "no rows in result set" {
		t.Fatalf("read item_cost: %v", err)
	}
	if avg == nil {
		return ""
	}
	return *avg
}

// grErr extracts the error string of a response.
func grErr(out map[string]any) string {
	msg, _ := out["error"].(string)
	return msg
}

// grPostAndAssert runs a happy-path draft → post and returns the receipt.
func grPostAndAssert(t *testing.T, e *grEnv, body map[string]any) map[string]any {
	t.Helper()
	status, out, id := grCreateDraft(t, e, body)
	if status != http.StatusCreated || id == "" {
		t.Fatalf("create draft expected 201, got %d: %v", status, out)
	}
	if out["data"].(map[string]any)["status"] != "draft" {
		t.Fatalf("new receipt = %v", out["data"])
	}
	status, out = grPost(t, e, id)
	if status != http.StatusOK {
		t.Fatalf("post expected 200, got %d: %v", status, out)
	}
	return out["data"].(map[string]any)
}

// TestGoodsReceipt_PostCreatesStockAndCost — posting creates the lot, the
// available purchase balance and a signed receipt move; the line stores
// base_qty/unit_cost/HNA+PPN and item_cost holds the average. No discounts,
// VAT included: 10 packs × 10000 → unit cost 10000.000000, avg 10000.000000.
func TestGoodsReceipt_PostCreatesStockAndCost(t *testing.T) {
	e := grSetup(t, "postcost", "081234620001")
	supplierID := grCreateSupplier(t, e, "SUP-GR-1")
	itemID, unitRowID := grItem(t, e, "GR-PC-1", nil)

	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	if receipt["status"] != "posted" {
		t.Fatalf("receipt status = %v", receipt["status"])
	}
	lines, _ := receipt["lines"].([]any)
	line := lines[0].(map[string]any)
	// Contract math: base_qty = 10 × 1 = 10 (column scale 3); unit cost =
	// 100000 ÷ 10 = 10000.
	if line["base_qty"] != "10.000" || line["unit_cost"] != "10000.000000" || line["hna_ppn_per_pack"] != "10000.00" {
		t.Fatalf("posted line = %v", line)
	}
	if line["lot_id"] == "" {
		t.Fatalf("lot missing: %v", line)
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "10.000" {
		t.Fatalf("balance = %q, want 10.000", got)
	}
	if got := grAvgCost(t, e, itemID); got != "10000.000000" {
		t.Fatalf("avg cost = %q, want 10000.000000", got)
	}
	var qty, cost string
	var moveType string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT quantity::text, unit_cost::text, move_type FROM inventory.stock_move
		WHERE ref_type = 'goods_receipt' AND ref_id = $1`, receipt["id"]).Scan(&qty, &cost, &moveType); err != nil {
		t.Fatalf("read move: %v", err)
	}
	if qty != "10.000" || cost != "10000.000000" || moveType != "receipt" {
		t.Fatalf("move = %s %s %s", qty, cost, moveType)
	}
}

// TestGoodsReceipt_MovingAverage — two receipts at different prices recompute
// the average: (10 × 10000 + 5 × 13000) ÷ 15 = 11000.000000.
func TestGoodsReceipt_MovingAverage(t *testing.T) {
	e := grSetup(t, "avg", "081234620002")
	supplierID := grCreateSupplier(t, e, "SUP-GR-2")
	itemID, unitRowID := grItem(t, e, "GR-AV-1", nil)

	grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	second := grDraftBody(e, supplierID, itemID, unitRowID, nil)
	line := second["lines"].([]map[string]any)[0]
	line["pack_qty"] = "5"
	line["pack_price"] = "13000"
	grPostAndAssert(t, e, second)
	// Contract math: (100000 + 65000) ÷ 15 = 11000.
	if got := grAvgCost(t, e, itemID); got != "11000.000000" {
		t.Fatalf("avg cost = %q, want 11000.000000", got)
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "15.000" {
		t.Fatalf("balance = %q, want 15.000", got)
	}
}

// TestGoodsReceipt_ProgramStockNotInCost — a program receipt adds stock but
// never touches item_cost (spec §4.1.5: HPP only for purchase).
func TestGoodsReceipt_ProgramStockNotInCost(t *testing.T) {
	e := grSetup(t, "prog", "081234620003")
	itemID, unitRowID := grItem(t, e, "GR-PG-1", nil)
	body := grDraftBody(e, "", itemID, unitRowID, map[string]any{"source": "program"})
	delete(body, "supplier_id")

	grPostAndAssert(t, e, body)
	if got := grBalanceQty(t, e, itemID, "program"); got != "10.000" {
		t.Fatalf("program balance = %q", got)
	}
	if got := grAvgCost(t, e, itemID); got != "" {
		t.Fatalf("program receipt must not create item_cost, got %q", got)
	}
}

// TestGoodsReceipt_ShortShelfLife — an expiry inside the 12-month minimum
// rejects posting with the per-line 409 report; the same line posts with an
// override reason; a past expiry is always 400.
func TestGoodsReceipt_ShortShelfLife(t *testing.T) {
	e := grSetup(t, "shelf", "081234620004")
	supplierID := grCreateSupplier(t, e, "SUP-GR-3")
	itemID, unitRowID := grItem(t, e, "GR-SL-1", map[string]any{"track_batch": true, "track_expiry": true})

	line := func(expiry string) map[string]any {
		return grDraftBody(e, supplierID, itemID, unitRowID, map[string]any{
			"lines": []map[string]any{{
				"item_id": itemID, "item_unit_id": unitRowID,
				"pack_qty": "10", "pack_price": "10000",
				"batch_no": "B-01", "expiry_date": expiry,
			}},
		})
	}

	// ~3 months of shelf life against the 12-month default minimum.
	shortExpiry := time.Now().UTC().AddDate(0, 3, 2).Format("2006-01-02")
	status, out, id := grCreateDraft(t, e, line(shortExpiry))
	if status != http.StatusCreated {
		t.Fatalf("draft expected 201, got %d: %v", status, out)
	}
	status, out = grPost(t, e, id)
	if status != http.StatusConflict || grErr(out) != "short_shelf_life" {
		t.Fatalf("post expected 409 short_shelf_life, got %d: %v", status, out)
	}
	shortLines, _ := out["lines"].([]any)
	if len(shortLines) != 1 {
		t.Fatalf("short lines = %v", out["lines"])
	}
	row := shortLines[0].(map[string]any)
	if row["line_id"] == "" || row["item"] != "GR-SL-1" || row["expiry_date"] == nil {
		t.Fatalf("short line = %v", row)
	}
	// Contract: whole months between received_on and expiry, partial month
	// floored — received today, expiry ~3m2d out → months_left = 3.
	wantMonths := 3
	if row["months_left"].(float64) != float64(wantMonths) {
		t.Fatalf("months_left = %v, want %d", row["months_left"], wantMonths)
	}

	// Override with a reason on the same draft (full line replace).
	patch := line(shortExpiry)
	patch["row_version"] = 1
	patch["lines"].([]map[string]any)[0]["short_shelf_life_reason"] = "stok kritis"
	status, out = grRequest(t, e, http.MethodPatch, "/api/v1/goods-receipts/"+id, patch)
	if status != http.StatusOK {
		t.Fatalf("patch expected 200, got %d: %v", status, out)
	}
	status, out = grPost(t, e, id)
	if status != http.StatusOK {
		t.Fatalf("post with reason expected 200, got %d: %v", status, out)
	}

	// A past expiry is 400 even with a reason.
	past := grDraftBody(e, supplierID, itemID, unitRowID, nil)
	past["lines"].([]map[string]any)[0] = map[string]any{
		"item_id": itemID, "item_unit_id": unitRowID,
		"pack_qty": "10", "pack_price": "10000",
		"batch_no": "B-02", "expiry_date": grToday(-1),
		"short_shelf_life_reason": "stok kritis",
	}
	status, out, id = grCreateDraft(t, e, past)
	if status != http.StatusCreated {
		t.Fatalf("draft 2 expected 201, got %d: %v", status, out)
	}
	status, out = grPost(t, e, id)
	if status != http.StatusBadRequest || grErr(out) != "expiry date is in the past" {
		t.Fatalf("past ED expected 400, got %d: %v", status, out)
	}
}

// TestGoodsReceipt_ReferencePriceAndProposal — posting a formulary item sets
// the reference price to HNA+PPN per pack and regenerates a
// reference_changed proposal at the 20% margin.
func TestGoodsReceipt_ReferencePriceAndProposal(t *testing.T) {
	env := ipSetup(t, "grref", "081234620005", nil)
	e := &grEnv{pool: env.pool, router: env.router, ctx: context.Background(),
		companyID: env.companyID, merchantID: env.merchantID, token: env.token}
	tx := runtimeTenantTx(t, e.ctx, env.pool, env.companyID, env.merchantID)
	if err := tx.QueryRow(e.ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room','LOC-GR-REF','Gudang Ref',ARRAY['warehouse']::text[])
		RETURNING id::text`, env.companyID, env.merchantID).Scan(&e.locationID); err != nil {
		t.Fatalf("insert location: %v", err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit location: %v", err)
	}
	supplierID := grCreateSupplier(t, e, "SUP-GR-REF")

	// Margin rule 20% first: the earlier reference price seeds a
	// margin_changed proposal; the receipt supersedes it.
	status, out := ipPutMargin(t, env, []map[string]any{{
		"pricing_category": "generic", "margin_percent": "20", "auto_approve": false,
	}})
	if status != http.StatusOK {
		t.Fatalf("put margin expected 200, got %d: %v", status, out)
	}

	// Receipt: 1 box (factor 100) at 100000 → HNA+PPN 100000.00 per pack.
	body := grDraftBody(e, supplierID, env.itemID, env.boxRowID, map[string]any{
		"lines": []map[string]any{{
			"item_id": env.itemID, "item_unit_id": env.boxRowID,
			"pack_qty": "1", "pack_content": "100", "pack_price": "100000",
			"batch_no": "B-REF", "expiry_date": grToday(700),
		}},
	})
	receipt := grPostAndAssert(t, e, body)
	line := receipt["lines"].([]any)[0].(map[string]any)
	if line["hna_ppn_per_pack"] != "100000.00" {
		t.Fatalf("hna = %v", line["hna_ppn_per_pack"])
	}

	// Formulary reference refreshed from the receipt.
	status, out = grRequest(t, e, http.MethodGet, "/api/v1/merchants/"+e.merchantID+"/formulary", nil)
	if status != http.StatusOK {
		t.Fatalf("list formulary expected 200, got %d: %v", status, out)
	}
	var ref map[string]any
	for _, r := range out["data"].([]any) {
		row := r.(map[string]any)
		if row["id"] == env.entryID {
			ref, _ = row["reference"].(map[string]any)
		}
	}
	if ref == nil || ref["amount"] != "100000.00" || ref["factor"] != "100.000" {
		t.Fatalf("reference = %v", ref)
	}

	// Contract math: 100000 ÷ 100 × 1.20 = 1200 per base unit × 100 = 120000.
	data := ipProposals(t, env, "?status=pending")
	if len(data) != 1 {
		t.Fatalf("pending proposals = %d: %v", len(data), data)
	}
	proposal := data[0].(map[string]any)
	if proposal["reason"] != "reference_changed" || proposal["proposed_amount"] != "1200.00" {
		t.Fatalf("proposal = %v", proposal)
	}
}

// TestGoodsReceipt_Void — a fully-intact receipt voids (balance back to zero,
// negative reversal move referencing the receipt move, average kept when the
// basis is back to zero); after part of the stock is used void is 409.
func TestGoodsReceipt_Void(t *testing.T) {
	e := grSetup(t, "void", "081234620006")
	supplierID := grCreateSupplier(t, e, "SUP-GR-4")
	itemID, unitRowID := grItem(t, e, "GR-VD-1", nil)

	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, map[string]any{"pack_qty": "10"}))
	receiptID := receipt["id"].(string)

	// First void: nothing used → allowed.
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/goods-receipts/"+receiptID+"/void",
		map[string]any{"reason": "salah posting"})
	if status != http.StatusOK || out["data"].(map[string]any)["status"] != "void" {
		t.Fatalf("void expected 200, got %d: %v", status, out)
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "0.000" {
		t.Fatalf("balance after void = %q, want 0.000", got)
	}
	// Basis is back to 0 → the average stays at 10000.000000 (spec §4.1.7).
	if got := grAvgCost(t, e, itemID); got != "10000.000000" {
		t.Fatalf("avg after void = %q", got)
	}
	var qty string
	var reversalOf *string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT quantity::text, reversal_of::text FROM inventory.stock_move
		WHERE ref_id = $1 AND move_type = 'reversal'`, receiptID).Scan(&qty, &reversalOf); err != nil {
		t.Fatalf("read reversal move: %v", err)
	}
	if qty != "-10.000" || reversalOf == nil || *reversalOf == "" {
		t.Fatalf("reversal move = qty %s reversal_of %v", qty, reversalOf)
	}

	// Second receipt, partially used → void must 409.
	body := grDraftBody(e, supplierID, itemID, unitRowID, nil)
	receipt2 := grPostAndAssert(t, e, body)
	if _, err := e.pool.Exec(e.ctx, `
		UPDATE inventory.stock_balance SET quantity = quantity - 4
		WHERE merchant_id = $1 AND item_id = $2 AND source = 'purchase' AND status = 'available'`,
		e.merchantID, itemID); err != nil {
		t.Fatalf("simulate usage: %v", err)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/goods-receipts/"+receipt2["id"].(string)+"/void",
		map[string]any{"reason": "coba lagi"})
	if status != http.StatusConflict || grErr(out) != "received stock was already used" {
		t.Fatalf("void after usage expected 409, got %d: %v", status, out)
	}
	// Void without a reason is 400.
	status, out, id := grCreateDraft(t, e, body)
	if status != http.StatusCreated {
		t.Fatalf("draft 3 expected 201, got %d: %v", status, out)
	}
	grPost(t, e, id)
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/goods-receipts/"+id+"/void", map[string]any{})
	if status != http.StatusBadRequest {
		t.Fatalf("void without reason expected 400, got %d: %v", status, out)
	}
}

// TestGoodsReceipt_CompanySupplier — a company-level supplier (merchant_id
// NULL) is created with the company-level permission and is visible from
// another merchant of the same company.
func TestGoodsReceipt_CompanySupplier(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantA, _, token, refreshCookie := seedAccountOwner(t, ctx, pool, router, "gr.company", "081234620007")
	merchantB := createTenantMerchant(t, ctx, pool, router, token, refreshCookie, companyID, "Cabang GR")

	raw, _ := json.Marshal(map[string]any{"code": "SUP-CO-GR", "name": "Supplier Pusat GR"})
	req := accountRequest(router, http.MethodPost, "/api/v1/suppliers", token, companyID, merchantA, raw)
	if req.Code != http.StatusCreated {
		t.Fatalf("company supplier expected 201, got %d: %s", req.Code, req.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(req.Body.Bytes(), &out)
	created := out["data"].(map[string]any)
	if created["merchant_id"] != nil {
		t.Fatalf("company supplier merchant_id = %v, want null", created["merchant_id"])
	}

	// Visible from merchant B's list (company + its own = just the company one).
	req = accountRequest(router, http.MethodGet, "/api/v1/merchants/"+merchantB+"/suppliers", token, companyID, merchantB, nil)
	if req.Code != http.StatusOK {
		t.Fatalf("list from B expected 200, got %d: %s", req.Code, req.Body.String())
	}
	out = map[string]any{}
	_ = json.Unmarshal(req.Body.Bytes(), &out)
	rows, _ := out["data"].([]any)
	if len(rows) != 1 || rows[0].(map[string]any)["code"] != "SUP-CO-GR" {
		t.Fatalf("merchant B suppliers = %v", out["data"])
	}

	// Merchant-level supplier creation for another merchant is refused.
	raw, _ = json.Marshal(map[string]any{"merchant_id": merchantB, "code": "SUP-X", "name": "X"})
	req = accountRequest(router, http.MethodPost, "/api/v1/suppliers", token, companyID, merchantA, raw)
	if req.Code != http.StatusForbidden {
		t.Fatalf("cross-merchant supplier expected 403, got %d: %s", req.Code, req.Body.String())
	}
}

// TestGoodsReceipt_LocationMustBeRoom — a non-room location of the SAME
// merchant (building) is rejected with 400 "location is not a room" (plan:
// "Lokasi harus ruangan merchant itu").
func TestGoodsReceipt_LocationMustBeRoom(t *testing.T) {
	e := grSetup(t, "room", "081234620009")
	supplierID := grCreateSupplier(t, e, "SUP-GR-6")
	itemID, unitRowID := grItem(t, e, "GR-RM-1", nil)

	// Building location of the same merchant.
	tx := runtimeTenantTx(t, e.ctx, e.pool, e.companyID, e.merchantID)
	var buildingID string
	if err := tx.QueryRow(e.ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name)
		VALUES ($1,$2,NULL,'building','LOC-GR-BLD','Gedang Utama') RETURNING id::text`,
		e.companyID, e.merchantID).Scan(&buildingID); err != nil {
		t.Fatalf("insert building: %v", err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit building: %v", err)
	}

	body := grDraftBody(e, supplierID, itemID, unitRowID, nil)
	body["location_id"] = buildingID
	status, out, _ := grCreateDraft(t, e, body)
	if status != http.StatusBadRequest || grErr(out) != "location is not a room" {
		t.Fatalf("non-room location expected 400, got %d: %v", status, out)
	}
}

// TestGoodsReceipt_RequiresPermission — without inventory.stock.receive the
// supplier list and receipt posting are 403.
func TestGoodsReceipt_RequiresPermission(t *testing.T) {
	e := grSetup(t, "perm", "081234620008")
	supplierID := grCreateSupplier(t, e, "SUP-GR-5")
	itemID, unitRowID := grItem(t, e, "GR-PM-1", nil)

	if _, err := e.pool.Exec(e.ctx, `DELETE FROM core.role_permission rp
		USING core.role r, core.permission p
		WHERE rp.role_id = r.id AND rp.permission_id = p.id
		  AND r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.receive'`); err != nil {
		t.Fatalf("revoke permission: %v", err)
	}
	status, out := grRequest(t, e, http.MethodGet, "/api/v1/merchants/"+e.merchantID+"/suppliers", nil)
	if status != http.StatusForbidden || !strings.Contains(grErr(out), "inventory.stock.receive") {
		t.Fatalf("list expected 403, got %d: %v", status, out)
	}
	status, out, _ = grCreateDraft(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	if status != http.StatusForbidden {
		t.Fatalf("create draft expected 403, got %d: %v", status, out)
	}
	// Restore for the pool's remaining lifetime hygiene.
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO core.role_permission (role_id, permission_id)
		SELECT r.id, p.id FROM core.role r, core.permission p
		WHERE r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.receive'
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("restore permission: %v", err)
	}
}
