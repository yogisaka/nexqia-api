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
	"time"
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

// TestStockReport_OnHandStatusFilter — ?status=quarantine returns only
// quarantine rows; without the param every status comes back; an unknown
// status is a 400 "invalid status filter" (spec §5.2 #5).
func TestStockReport_OnHandStatusFilter(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "onhandst", "+628111000007")
	lot := opsSeedLot(t, ctx, e, "LOT-ST1", opsED(400))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 4000) // 4.000 available
	// One quarantine balance at the same location/lot/source.
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.stock_balance
			(company_id, merchant_id, location_id, item_id, lot_id, source, status, quantity)
		VALUES ($1,$2,$3,$4,$5,'purchase','quarantine','1.500')`,
		e.w.companyStr, e.w.merchantStr, e.w.location, e.w.item, lot); err != nil {
		t.Fatalf("seed quarantine balance: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit quarantine: %v", err)
	}
	path := "/api/v1/merchants/" + e.w.merchantStr + "/stock"

	// ?status=quarantine → exactly the quarantine row.
	status, out := opsRequest(t, e, http.MethodGet, path+"?status=quarantine", nil)
	if status != http.StatusOK {
		t.Fatalf("GET status=quarantine: %d %v", status, opsErr(out))
	}
	rows := repOnHandRows(t, out)
	if len(rows) != 1 || rows[0]["status"] != "quarantine" {
		t.Fatalf("want 1 quarantine row, got %v", out)
	}
	repRatEq(t, rows[0]["quantity"].(string), "1.500")

	// Without the param → both rows (old behavior, ListStockOnHand).
	status, out = opsRequest(t, e, http.MethodGet, path, nil)
	if status != http.StatusOK || len(repOnHandRows(t, out)) != 2 {
		t.Fatalf("no param: %d %v", status, out)
	}

	// Unknown status → 400 invalid status filter.
	status, out = opsRequest(t, e, http.MethodGet, path+"?status=weird", nil)
	if status != http.StatusBadRequest || opsErr(out) != "invalid status filter" {
		t.Fatalf("bad status: %d %v", status, out)
	}
}

// TestRecallReport — company-wide lot recall report (spec §5.2 #3): balances
// per merchant/location/status across two merchants of one company, issue
// usages with patient_name only for admission-referenced moves, and no leak
// of another company's lot.
func TestRecallReport(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "recall", "+628111000005")
	lot := opsSeedLot(t, ctx, e, "LOT-RC1", opsED(400))
	opsSeedBalanceAt(t, ctx, e, e.w.location, lot, "purchase", 4000) // 4.000 available

	// Merchant 2 of the same company + one quarantine balance. The location
	// and balance inserts need the tx pinned to merchant 2 (RLS).
	tx := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	var m2Str string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.merchant (company_id, code, name)
		VALUES ($1, 'M-RC2', 'RS Uji Cabang') RETURNING id::text`,
		e.w.companyStr).Scan(&m2Str); err != nil {
		t.Fatalf("seed merchant 2: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit merchant 2: %v", err)
	}
	tx2 := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, m2Str)
	var loc2Str string
	if err := tx2.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, kind, code, name, functions)
		VALUES ($1,$2,'room','GUD-RC2','Gudang Cabang', ARRAY['warehouse']::text[]) RETURNING id::text`,
		e.w.companyStr, m2Str).Scan(&loc2Str); err != nil {
		t.Fatalf("seed location 2: %v", err)
	}
	if _, err := tx2.Exec(ctx, `
		INSERT INTO inventory.stock_balance
			(company_id, merchant_id, location_id, item_id, lot_id, source, status, quantity)
		VALUES ($1,$2,$3,$4,$5,'purchase','quarantine','1.500')`,
		e.w.companyStr, m2Str, loc2Str, e.w.item, lot); err != nil {
		t.Fatalf("seed merchant 2 balance: %v", err)
	}
	if err := tx2.Commit(ctx); err != nil {
		t.Fatalf("commit merchant 2 balance: %v", err)
	}

	// Issue usages: admission-referenced move (patient) then a plain usage.
	tx = stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	var personID, deptID, admissionID string
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.person (company_id, full_name, gender)
		VALUES ($1, 'Pasien Uji Recall', 'female') RETURNING id::text`,
		e.w.companyStr).Scan(&personID); err != nil {
		t.Fatalf("seed person: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.department (company_id, merchant_id, code, name)
		VALUES ($1,$2,'RCL01','Poli Recall') RETURNING id::text`,
		e.w.companyStr, e.w.merchantStr).Scan(&deptID); err != nil {
		t.Fatalf("seed department: %v", err)
	}
	if err := tx.QueryRow(ctx, `
		INSERT INTO operations.admission
			(company_id, merchant_id, visit_no, person_id, admission_type, department_id)
		VALUES ($1,$2,'RCL-0001',$3,'outpatient',$4) RETURNING id::text`,
		e.w.companyStr, e.w.merchantStr, personID, deptID).Scan(&admissionID); err != nil {
		t.Fatalf("seed admission: %v", err)
	}
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id,
			 source, status, quantity, ref_type, ref_id, created_by)
		VALUES ($1,$2,$3,'issue',$4,$5,$6,'purchase','available','-2.000','admission',$7,$8)`,
		e.w.companyStr, e.w.merchantStr, e.w.day.Format("2006-01-02"), e.w.location,
		e.w.item, lot, admissionID, e.w.userStr); err != nil {
		t.Fatalf("seed admission issue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit admission issue: %v", err)
	}
	tx = stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	if _, err := tx.Exec(ctx, `
		INSERT INTO inventory.stock_move
			(company_id, merchant_id, move_date, move_type, location_id, item_id, lot_id,
			 source, status, quantity, ref_type, created_by)
		VALUES ($1,$2,$3,'issue',$4,$5,$6,'purchase','available','-1.000','usage',$7)`,
		e.w.companyStr, e.w.merchantStr, e.w.day.Format("2006-01-02"), e.w.location,
		e.w.item, lot, e.w.userStr); err != nil {
		t.Fatalf("seed plain issue: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit plain issue: %v", err)
	}

	path := "/api/v1/lots/" + lot.String() + "/recall-report"
	status, out := opsRequest(t, e, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET recall-report: %d %v", status, opsErr(out))
	}
	data, _ := out["data"].(map[string]any)
	lotB, _ := data["lot"].(map[string]any)
	if lotB["batch_no"] != "LOT-RC1" {
		t.Fatalf("lot block: %v", lotB)
	}
	balances, _ := data["balances"].([]any)
	if len(balances) != 2 {
		t.Fatalf("want 2 balances, got %v", data)
	}
	byStatus := map[string]map[string]any{}
	for _, b := range balances {
		bm, _ := b.(map[string]any)
		byStatus[bm["status"].(string)] = bm
	}
	avail := byStatus["available"]
	if avail == nil || avail["merchant_id"] != e.w.merchant.String() ||
		avail["location_name"] != "Gudang Ops" {
		t.Fatalf("available balance: %v", avail)
	}
	repRatEq(t, avail["quantity"].(string), "4.000")
	quar := byStatus["quarantine"]
	if quar == nil || quar["merchant_id"] != m2Str || quar["location_name"] != "Gudang Cabang" {
		t.Fatalf("quarantine balance: %v", quar)
	}
	repRatEq(t, quar["quantity"].(string), "1.500")

	usages, _ := data["usages"].([]any)
	if len(usages) != 2 {
		t.Fatalf("want 2 usages, got %v", data)
	}
	// Both moves land in the same created_at (same test second); the id DESC
	// tiebreaker puts the later-inserted plain usage first.
	u0, _ := usages[0].(map[string]any)
	if u0["move_type"] != "issue" || u0["patient_name"] != nil {
		t.Fatalf("plain usage: %v", u0)
	}
	repRatEq(t, u0["quantity"].(string), "-1.000")
	if u0["created_by_name"] == nil || u0["moved_at"] == nil {
		t.Fatalf("usage meta missing: %v", u0)
	}
	u1, _ := usages[1].(map[string]any)
	if u1["patient_name"] != "Pasien Uji Recall" {
		t.Fatalf("admission usage must have patient_name: %v", u1)
	}
	repRatEq(t, u1["quantity"].(string), "-2.000")

	// A lot of another company does not leak: 404 lot not found.
	e2 := opsSetup(t, "recall2", "+628111000006")
	lot2 := opsSeedLot(t, ctx, e2, "LOT-RC-OTHER", opsED(400))
	status, out = opsRequest(t, e, http.MethodGet,
		"/api/v1/lots/"+lot2.String()+"/recall-report", nil)
	if status != http.StatusNotFound || opsErr(out) != "lot not found" {
		t.Fatalf("cross-company lot: %d %v", status, out)
	}
}

// TestStockReport_NearExpiryBud — opened containers merge into the
// near-expiry report as a separate bud_containers list (spec
// 2026-10-20-inventory-17b §7.2 #1) with the same bucket rules as the lot
// buckets; bucket quantities stay IDENTICAL with/without containers (the
// container contents still sit in the lot balance — anti double-count).
func TestStockReport_NearExpiryBud(t *testing.T) {
	ctx := context.Background()
	e := opsSetup(t, "edbud", "+628111000008")
	path := "/api/v1/merchants/" + e.w.merchantStr + "/stock/near-expiry"

	// Baseline buckets WITHOUT containers: one expired and one within_1 lot.
	expLot := opsSeedLot(t, ctx, e, "LOT-BUD-EXP", opsED(-5))
	opsSeedBalanceAt(t, ctx, e, e.w.location, expLot, "purchase", 1000) // 1 unit
	in1Lot := opsSeedLot(t, ctx, e, "LOT-BUD-1", opsED(30))
	opsSeedBalanceAt(t, ctx, e, e.w.location, in1Lot, "purchase", 1000) // 1 unit
	status, before := opsRequest(t, e, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET near-expiry baseline: %d %v", status, opsErr(before))
	}

	// Vaccine item with BUD rules (open 24 h) plus lot and balance to feed
	// the containers (seeding mirrors TestColdChainRoutesHTTP).
	seed := stockRuntimeTx(t, ctx, e.w.pool, e.w.companyStr, e.w.merchantStr)
	vax := ccSeedVaccineItem(t, ctx, seed, e.w, "EDB")
	world := *e.w
	world.item = vax
	vaxLot := stockSeedLot(t, ctx, seed, &world, "LOT-BUD-VAX", nil, false)
	ccSeedBalance(t, ctx, seed, &world, vax, vaxLot, 3_000) // 3 units
	if err := seed.Commit(ctx); err != nil {
		t.Fatalf("commit seed: %v", err)
	}

	now := time.Now()
	openAt := func(bud time.Time) string {
		return bud.Add(-24 * time.Hour).Format(time.RFC3339) // open rule = 24 h
	}
	openContainer := func(openedAt, qty string) string {
		st, out := opsRequest(t, e, http.MethodPost,
			"/api/v1/merchants/"+e.w.merchantStr+"/opened-containers", map[string]any{
				"item_id": vax.String(), "lot_id": vaxLot.String(),
				"location_id": e.w.location.String(),
				"opened_at":   openedAt, "remaining_base_qty": qty,
			})
		if st != http.StatusCreated {
			t.Fatalf("opened container: %d %v", st, opsErr(out))
		}
		id, _ := out["data"].(map[string]any)["id"].(string)
		return id
	}
	// bud = now − 48 h: certainly before the merchant day → expired.
	expiredID := openContainer(openAt(now.Add(-48*time.Hour)), "1")
	// bud = now + 24 h: still within the m1 window → within_1.
	in1ID := openContainer(openAt(now.Add(24*time.Hour)), "1")
	// This one is fully consumed via usage → finished → must NOT be reported.
	finishedID := openContainer(openAt(now.Add(24*time.Hour)), "1")
	st, out := opsRequest(t, e, http.MethodPost, opsUsagePath(e), map[string]any{
		"location_id": e.w.location.String(), "item_id": vax.String(),
		"quantity": "1", "reason": "habis dipakai",
		"opened_container_id": finishedID, "vvm_stage": 1,
	})
	if st != http.StatusCreated {
		t.Fatalf("usage from container: %d %v", st, opsErr(out))
	}

	status, out = opsRequest(t, e, http.MethodGet, path, nil)
	if status != http.StatusOK {
		t.Fatalf("GET near-expiry: %d %v", status, opsErr(out))
	}
	data, _ := out["data"].(map[string]any)
	// Anti double-count: bucket quantities identical to the sans-container run.
	for bucket, want := range map[string]string{
		"expired": "1.000", "within_1": "1.000", "within_2": "0",
	} {
		b, okB := data[bucket].(map[string]any)
		if !okB {
			t.Fatalf("bucket %s missing: %v", bucket, data)
		}
		beforeB, _ := before["data"].(map[string]any)[bucket].(map[string]any)
		if b["quantity"] != want || beforeB["quantity"] != want {
			t.Fatalf("%s quantity: got %v (before %v), want %s", bucket, b["quantity"], beforeB["quantity"], want)
		}
	}
	// bud_containers: exactly the two open containers, bucketed like the lots.
	list, okL := data["bud_containers"].([]any)
	if !okL || len(list) != 2 {
		t.Fatalf("bud_containers = %v, want exactly 2", data["bud_containers"])
	}
	byID := map[string]map[string]any{}
	for _, raw := range list {
		m, _ := raw.(map[string]any)
		byID[m["container_id"].(string)] = m
	}
	if m := byID[expiredID]; m == nil || m["bucket"] != "expired" {
		t.Fatalf("container %s: %v, want expired", expiredID, m)
	}
	if m := byID[in1ID]; m == nil || m["bucket"] != "within_1" {
		t.Fatalf("container %s: %v, want within_1", in1ID, m)
	}
	if _, gone := byID[finishedID]; gone {
		t.Fatalf("finished container %s must not be reported", finishedID)
	}
	for _, id := range []string{expiredID, in1ID} {
		m := byID[id]
		if m["remaining_base_qty"] != "1.000" {
			t.Fatalf("container %s remaining: %v", id, m["remaining_base_qty"])
		}
		if m["item_name"] != "Vaksin Uji EDB" || m["batch_no"] != "LOT-BUD-VAX" {
			t.Fatalf("container %s names: %v", id, m)
		}
		if _, err := time.Parse(time.RFC3339, m["bud_at"].(string)); err != nil {
			t.Fatalf("container %s bud_at not RFC3339: %v", id, m["bud_at"])
		}
	}
}
