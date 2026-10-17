//go:build integration

package server

// Integration tests for the stock reports API (plan 2026-10-07-inventory-17a
// Task 6, spec §4.6). HTTP-level tests against the real router; seeding
// reuses the stock ops test environment (opsSetup, opsSeedLot,
// opsSeedBalanceAt, opsRequest — stock_ops_test.go).

import (
	"context"
	"fmt"
	"math/big"
	"net/http"
	"testing"
)

// repRatEq asserts two decimal strings are the same number (scale may
// differ between renders).
func repRatEq(t *testing.T, got, want string) {
	t.Helper()
	g, okG := new(big.Rat).SetString(got)
	w, okW := new(big.Rat).SetString(want)
	if !okG || !okW || g.Cmp(w) != 0 {
		t.Fatalf("value mismatch: got %s, want %s", got, want)
	}
}

func repOnHandRows(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	data, _ := out["data"].(map[string]any)
	rows, _ := data["rows"].([]any)
	parsed := make([]map[string]any, 0, len(rows))
	for _, r := range rows {
		m, _ := r.(map[string]any)
		parsed = append(parsed, m)
	}
	return parsed
}

// TestStockReport_OnHandValue — on-hand rows per lot with qty × avg_cost
// value and the total; filters by location and status.
func TestStockReport_OnHandValue(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "onhand", "+628111000001")
	lot := opsSeedLot(t, ctx, e, "LOT-OH1", opsED(400))
	// avg_cost = 1500/unit (inventory.item_cost, numeric(19,6)).
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.item_cost (company_id, merchant_id, item_id, avg_cost)
		VALUES ($1,$2,$3,1500)`,
		e.w.companyStr, e.w.merchantStr, e.w.item.String()); err != nil {
		t.Fatalf("seed item_cost: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cost: %v", err)
	}
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 10000) // 10.000 units

	path := "/api/v1/merchants/" + e.w.merchantStr + "/stock"
	status, out := opsRequest(t, e, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET stock: %d %v", status, opsErr(out))
	}
	rows := repOnHandRows(t, out)
	if len(rows) != 1 {
		t.Fatalf("want 1 row, got %d: %v", len(rows), out)
	}
	row := rows[0]
	if row["location"] != e.w.location.String() {
		t.Fatalf("location: got %v", row["location"])
	}
	item := row["item"].(map[string]any)
	if item["code"] != "ITM-OPS" || item["name"] != "Obat Uji Ops" || item["base_unit"] != "tablet" {
		t.Fatalf("item block: %v", item)
	}
	lotB := row["lot"].(map[string]any)
	if lotB["batch_no"] != "LOT-OH1" || lotB["is_recalled"] != false || lotB["expiry_date"] == nil {
		t.Fatalf("lot block: %v", lotB)
	}
	if row["source"] != "purchase" || row["status"] != "available" {
		t.Fatalf("source/status: %v", row)
	}
	if row["quantity"] != "10.000" {
		t.Fatalf("quantity: %v", row["quantity"])
	}
	repRatEq(t, row["avg_cost"].(string), "1500.000000")
	repRatEq(t, row["value"].(string), "15000.00") // 10.000 × 1500 = 15000
	data, _ := out["data"].(map[string]any)
	repRatEq(t, data["total_value"].(string), "15000.00")

	// Filters: unknown status → 400, q miss → empty.
	status, out = opsRequest(t, e, http.MethodGet, path+"?status=weird", nil)
	if status != http.StatusBadRequest {
		t.Fatalf("bad status filter: %d", status)
	}
	status, out = opsRequest(t, e, http.MethodGet, path+"?q=nomatch", nil)
	if status != http.StatusOK || len(repOnHandRows(t, out)) != 0 {
		t.Fatalf("q filter: %d %v", status, out)
	}
	// Other location excluded by location_id filter.
	status, out = opsRequest(t, e, http.MethodGet, path+"?location_id="+e.loc2.String(), nil)
	if status != http.StatusOK || len(repOnHandRows(t, out)) != 0 {
		t.Fatalf("location filter: %d %v", status, out)
	}
}

// TestStockReport_StockCardRunningBalance — opening balance plus moves in
// (move_date, created_at) order with a running balance.
func TestStockReport_StockCardRunningBalance(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "card", "+628111000002")
	lot := opsSeedLot(t, ctx, e, "LOT-SC1", opsED(400))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 10000) // +10
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 5000)  // +5
	// Issue 3 via the API.
	status, out := opsRequest(t, e, http.MethodPost, opsUsagePath(e), map[string]any{
		"location_id": e.w.location.String(),
		"item_id":     e.w.item.String(),
		"quantity":    "3",
		"reason":      "card test",
	})
	if status != http.StatusCreated {
		t.Fatalf("usage issue: %d %v", status, opsErr(out))
	}
	path := fmt.Sprintf("/api/v1/merchants/%s/stock-card?item_id=%s", e.w.merchantStr, e.w.item.String())
	status, out = opsRequest(t, e, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET stock-card: %d %v", status, opsErr(out))
	}
	data, _ := out["data"].(map[string]any)
	repRatEq(t, data["opening_balance"].(string), "0")
	moves, _ := data["moves"].([]any)
	if len(moves) != 3 {
		t.Fatalf("want 3 moves, got %d", len(moves))
	}
	want := []string{"10.000", "15.000", "12.000"}
	for i, m := range moves {
		mm, _ := m.(map[string]any)
		repRatEq(t, mm["balance"].(string), want[i]) // running: +10, +5, −3
	}
	if moves[1].(map[string]any)["move_type"] != "receipt" {
		t.Fatalf("move order: %v", moves)
	}
	// from tomorrow → whole history becomes the opening balance.
	path = fmt.Sprintf("/api/v1/merchants/%s/stock-card?item_id=%s&from=%s",
		e.w.merchantStr, e.w.item.String(), opsED(1).Format("2006-01-02"))
	status, out = opsRequest(t, e, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET stock-card from: %d %v", status, opsErr(out))
	}
	data, _ = out["data"].(map[string]any)
	repRatEq(t, data["opening_balance"].(string), "12.000")
	if m, _ := data["moves"].([]any); len(m) != 0 {
		t.Fatalf("want no moves after from, got %d", len(m))
	}
}

// TestStockReport_NearExpiryBuckets — expired / within_1 / within_2 buckets
// with the merchant defaults (3 and 6 months).
func TestStockReport_NearExpiryBuckets(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "ed", "+628111000003")
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.item_cost (company_id, merchant_id, item_id, avg_cost)
		VALUES ($1,$2,$3,1000)`,
		e.w.companyStr, e.w.merchantStr, e.w.item.String()); err != nil {
		t.Fatalf("seed item_cost: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit cost: %v", err)
	}
	cases := []struct {
		batch  string
		days   int
		bucket string
	}{
		{"LOT-ED-EXP", -5, "expired"}, // ED < hari ini
		{"LOT-ED-1", 30, "within_1"},  // ≤ 3 bulan
		{"LOT-ED-2", 100, "within_2"}, // ≤ 6 bulan, > 3 bulan
		{"LOT-ED-FAR", 300, ""},       // di luar semua bucket
	}
	for _, c := range cases {
		lot := opsSeedLot(t, ctx, e, c.batch, opsED(c.days))
		opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 1000) // 1 unit
	}
	status, out := opsRequest(t, e, http.MethodGet,
		"/api/v1/merchants/"+e.w.merchantStr+"/stock/near-expiry", nil)
	if status != http.StatusOK {
		t.Fatalf("GET near-expiry: %d %v", status, opsErr(out))
	}
	data, _ := out["data"].(map[string]any)
	for bucket, wantQty := range map[string]string{
		"expired": "1.000", "within_1": "1.000", "within_2": "1.000",
	} {
		b, okB := data[bucket].(map[string]any)
		if !okB {
			t.Fatalf("bucket %s missing: %v", bucket, data)
		}
		if b["quantity"] != wantQty {
			t.Fatalf("%s quantity: got %v want %s", bucket, b["quantity"], wantQty)
		}
		repRatEq(t, b["value"].(string), "1000.00") // 1 unit × avg_cost 1000
	}
}

// TestStockReport_Reorder — stock levels below the location minimum appear
// with suggested = max − available; levels at/above minimum are skipped.
func TestStockReport_Reorder(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "reorder", "+628111000004")
	lot := opsSeedLot(t, ctx, e, "LOT-RO1", opsED(400))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 40000) // 40.000 available
	lowPath := "/api/v1/merchants/" + e.w.merchantStr + "/stock-levels"
	status, out := opsRequest(t, e, http.MethodPut, lowPath, map[string]any{
		"levels": []map[string]any{{
			"location_id": e.w.location.String(),
			"item_id":     e.w.item.String(),
			"min_qty":     "100",
			"max_qty":     "500",
		}},
	})
	if status != http.StatusOK {
		t.Fatalf("PUT stock-levels: %d %v", status, opsErr(out))
	}
	status, out = opsRequest(t, e, http.MethodGet,
		"/api/v1/merchants/"+e.w.merchantStr+"/stock/reorder", nil)
	if status != http.StatusOK {
		t.Fatalf("GET reorder: %d %v", status, opsErr(out))
	}
	rows, _ := out["data"].([]any)
	if len(rows) != 1 {
		t.Fatalf("want 1 reorder row, got %d: %v", len(rows), out)
	}
	row, _ := rows[0].(map[string]any)
	item := row["item"].(map[string]any)
	if item["code"] != "ITM-OPS" {
		t.Fatalf("item block: %v", item)
	}
	if row["location_id"] != e.w.location.String() {
		t.Fatalf("location_id: %v", row["location_id"])
	}
	repRatEq(t, row["min_qty"].(string), "100.000")
	repRatEq(t, row["max_qty"].(string), "500.000")
	repRatEq(t, row["available_qty"].(string), "40.000")
	repRatEq(t, row["suggested"].(string), "460.000") // max − saldo

	// Raise the balance above the minimum → the row disappears.
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 100000) // +100 → 140
	status, out = opsRequest(t, e, http.MethodGet,
		"/api/v1/merchants/"+e.w.merchantStr+"/stock/reorder", nil)
	if status != http.StatusOK {
		t.Fatalf("GET reorder 2: %d %v", status, opsErr(out))
	}
	if rows, _ = out["data"].([]any); len(rows) != 0 {
		t.Fatalf("want no reorder rows above min, got %d", len(rows))
	}
}
