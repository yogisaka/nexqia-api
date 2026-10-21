//go:build integration

package server_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestStockOut_* covers the P2 outbound value flows (plan
// 2026-10-20-inventory-17b-p2 Task 2, spec §5): supplier returns reverse the
// moving-average HPP exactly (consignment never touches it) and voids restore
// stock + HPP; destructions enforce the narcotics four-eyes report and never
// move HPP; inter-merchant transfers ship the sender's avg cost and restore
// both stock and HPP on cancel; daily numbers stay per-merchant (SRET-/DSTR-)
// and per-company (IMT-).

// soRequestAs sends a request with an explicit token and active merchant
// (X-Merchant-ID).
func soRequestAs(t *testing.T, e *grEnv, token, merchantID, method, path string, body map[string]any) (int, map[string]any) {
	t.Helper()
	var raw []byte
	if body != nil {
		raw, _ = json.Marshal(body)
	}
	rec := accountRequest(e.router, method, path, token, e.companyID, merchantID, raw)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// soTokenFor mints an access token whose active merchant is merchantID
// (switch-merchant, pola mfa_test) — AuthMerchantID comes from the token's
// "mid" claim, so receiver-side guards need a switched session.
func soTokenFor(t *testing.T, e *grEnv, refreshCookie *http.Cookie, merchantID string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "/api/v1/auth/switch-merchant",
		bytes.NewReader([]byte(`{"merchant_id":"`+merchantID+`"}`)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+e.token)
	req.Header.Set("X-Company-ID", e.companyID)
	req.Header.Set("X-Merchant-ID", e.merchantID)
	req.AddCookie(refreshCookie)
	rec := httptest.NewRecorder()
	e.router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("switch-merchant expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	token, _ := out["data"].(map[string]any)["token"].(string)
	if token == "" {
		t.Fatalf("switch-merchant returned no token: %s", rec.Body.String())
	}
	return token
}

// soBalanceQty reads the summed available quantity of a source at an
// arbitrary merchant ("" when no row exists).
func soBalanceQty(t *testing.T, e *grEnv, merchantID, itemID, source string) string {
	t.Helper()
	var qty *string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT SUM(quantity)::text FROM inventory.stock_balance
		WHERE merchant_id = $1 AND item_id = $2 AND source = $3 AND status = 'available'`,
		merchantID, itemID, source).Scan(&qty); err != nil && err.Error() != "no rows in result set" {
		t.Fatalf("read balance: %v", err)
	}
	if qty == nil {
		return ""
	}
	return *qty
}

// soAvgCost reads an arbitrary merchant's stored avg cost ("" when absent).
func soAvgCost(t *testing.T, e *grEnv, merchantID, itemID string) string {
	t.Helper()
	var avg *string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT avg_cost::text FROM inventory.item_cost
		WHERE merchant_id = $1 AND item_id = $2`, merchantID, itemID).Scan(&avg); err != nil && err.Error() != "no rows in result set" {
		t.Fatalf("read item_cost: %v", err)
	}
	if avg == nil {
		return ""
	}
	return *avg
}

// soTwoLotReceipts receives 10 @ 100 (lot B-SR-1) then 10 @ 200 (lot B-SR-2);
// contract math: avg = (10×100 + 10×200) / 20 = 150. Returns the second lot's id.
func soTwoLotReceipts(t *testing.T, e *grEnv, supplierID, itemID, unitRowID string) string {
	t.Helper()
	first := grDraftBody(e, supplierID, itemID, unitRowID, map[string]any{
		"lines": []map[string]any{{
			"item_id": itemID, "item_unit_id": unitRowID,
			"pack_qty": "10", "pack_price": "100",
			"batch_no": "B-SR-1", "expiry_date": grToday(700),
		}},
	})
	grPostAndAssert(t, e, first)
	second := grDraftBody(e, supplierID, itemID, unitRowID, map[string]any{
		"lines": []map[string]any{{
			"item_id": itemID, "item_unit_id": unitRowID,
			"pack_qty": "10", "pack_price": "200",
			"batch_no": "B-SR-2", "expiry_date": grToday(700),
		}},
	})
	receipt2 := grPostAndAssert(t, e, second)
	return receipt2["lines"].([]any)[0].(map[string]any)["lot_id"].(string)
}

// soCreateReturn creates a supplier-return draft; returns (status, body, id).
func soCreateReturn(t *testing.T, e *grEnv, supplierID, itemID, lotID, baseQty string, extra map[string]any) (int, map[string]any, string) {
	t.Helper()
	body := map[string]any{
		"supplier_id": supplierID, "reason": "barang rusak dikirim supplier",
		"lines": []map[string]any{{
			"item_id": itemID, "lot_id": lotID, "source": "purchase", "base_qty": baseQty,
		}},
	}
	for k, v := range extra {
		body[k] = v
	}
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/supplier-returns", body)
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id, _ = data["id"].(string)
	}
	return status, out, id
}

// TestSupplierReturnReversesCost — receiving 10@100 then 10@200 gives avg 150;
// returning 4 from the second lot @200 moves the average to the exact
// (20×150 − 4×200)/16 = 137.5 and writes one negative supplier_return move.
func TestSupplierReturnReversesCost(t *testing.T) {
	e := grSetup(t, "sretcost", "081234620101")
	supplierID := grCreateSupplier(t, e, "SUP-SR-1")
	itemID, unitRowID := grItem(t, e, "SR-TC-1", nil)
	lot2 := soTwoLotReceipts(t, e, supplierID, itemID, unitRowID)
	if got := grAvgCost(t, e, itemID); got != "150.000000" {
		t.Fatalf("avg before return = %q, want 150.000000", got)
	}

	status, out, id := soCreateReturn(t, e, supplierID, itemID, lot2, "4", nil)
	if status != http.StatusCreated || id == "" {
		t.Fatalf("create return expected 201, got %d: %v", status, out)
	}
	data := out["data"].(map[string]any)
	if !strings.HasPrefix(data["return_no"].(string), "SRET-") {
		t.Fatalf("return_no = %v", data["return_no"])
	}
	// Prefill: the second lot's last receipt move cost.
	line := data["lines"].([]any)[0].(map[string]any)
	if line["unit_cost"] != "200.000000" {
		t.Fatalf("prefilled unit_cost = %v, want 200.000000", line["unit_cost"])
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/supplier-returns/"+id+"/post", nil)
	if status != http.StatusOK || out["data"].(map[string]any)["status"] != "posted" {
		t.Fatalf("post expected 200 posted, got %d: %v", status, out)
	}
	// Contract math: (20×150 − 4×200)/16 = 2200/16 = 137.5.
	if got := grAvgCost(t, e, itemID); got != "137.500000" {
		t.Fatalf("avg after return = %q, want 137.500000", got)
	}
	if got := soBalanceQty(t, e, e.merchantID, itemID, "purchase"); got != "16.000" {
		t.Fatalf("balance after return = %q, want 16.000", got)
	}
	var qty, moveType string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT quantity::text, move_type FROM inventory.stock_move
		WHERE ref_type = 'supplier_return' AND ref_id = $1`, id).Scan(&qty, &moveType); err != nil {
		t.Fatalf("read return move: %v", err)
	}
	if qty != "-4.000" || moveType != "supplier_return" {
		t.Fatalf("move = %s %s", qty, moveType)
	}
}

// TestSupplierReturnConsignmentNoCost — a consignment return decreases the
// consignment balance but never creates/touches item_cost.
func TestSupplierReturnConsignmentNoCost(t *testing.T) {
	e := grSetup(t, "sretcons", "081234620102")
	supplierID := grCreateSupplier(t, e, "SUP-SR-2")
	itemID, unitRowID := grItem(t, e, "SR-CC-1", nil)
	body := grDraftBody(e, supplierID, itemID, unitRowID, map[string]any{"source": "consignment"})
	receipt := grPostAndAssert(t, e, body)
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)
	if got := grAvgCost(t, e, itemID); got != "" {
		t.Fatalf("consignment receipt created item_cost %q", got)
	}

	status, out, id := soCreateReturn(t, e, supplierID, itemID, lotID, "4", map[string]any{
		"lines": []map[string]any{{
			"item_id": itemID, "lot_id": lotID, "source": "consignment", "base_qty": "4",
		}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create consignment return expected 201, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/supplier-returns/"+id+"/post", nil)
	if status != http.StatusOK {
		t.Fatalf("post expected 200, got %d: %v", status, out)
	}
	if got := grAvgCost(t, e, itemID); got != "" {
		t.Fatalf("consignment return touched item_cost: %q", got)
	}
	if got := grBalanceQty(t, e, itemID, "consignment"); got != "6.000" {
		t.Fatalf("consignment balance = %q, want 6.000", got)
	}
}

// TestSupplierReturnVoid — voiding a posted return restores the stock and
// re-applies the HPP (back to 150.000000); posted_at survives as NOT NULL
// (chk_supplier_return_posted is draft-only).
func TestSupplierReturnVoid(t *testing.T) {
	e := grSetup(t, "sretvoid", "081234620103")
	supplierID := grCreateSupplier(t, e, "SUP-SR-3")
	itemID, unitRowID := grItem(t, e, "SR-VW-1", nil)
	lot2 := soTwoLotReceipts(t, e, supplierID, itemID, unitRowID)
	_, _, id := soCreateReturn(t, e, supplierID, itemID, lot2, "4", nil)
	if _, out := grRequest(t, e, http.MethodPost, "/api/v1/supplier-returns/"+id+"/post", nil); out["data"].(map[string]any)["status"] != "posted" {
		t.Fatalf("post expected posted, got %v", out)
	}

	// Void without a reason is 400 with the exact contract message.
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/supplier-returns/"+id+"/void", map[string]any{})
	if status != http.StatusBadRequest || grErr(out) != "void reason required" {
		t.Fatalf("void without reason expected 400 void reason required, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/supplier-returns/"+id+"/void",
		map[string]any{"reason": "salah lot"})
	if status != http.StatusOK || out["data"].(map[string]any)["status"] != "void" {
		t.Fatalf("void expected 200 void, got %d: %v", status, out)
	}
	voided := out["data"].(map[string]any)
	// posted_at stays set: the posted check is draft-only.
	if voided["posted_at"] == nil {
		t.Fatalf("posted_at lost on void: %v", voided)
	}
	// Contract math: void re-applies 4 @ 200 on the 16-basis → back to 150.
	if got := grAvgCost(t, e, itemID); got != "150.000000" {
		t.Fatalf("avg after void = %q, want 150.000000", got)
	}
	if got := soBalanceQty(t, e, e.merchantID, itemID, "purchase"); got != "20.000" {
		t.Fatalf("balance after void = %q, want 20.000", got)
	}
}

// TestSupplierReturnForbidden — without inventory.stock.return creation is 403.
func TestSupplierReturnForbidden(t *testing.T) {
	e := grSetup(t, "sretperm", "081234620104")
	supplierID := grCreateSupplier(t, e, "SUP-SR-4")
	itemID, unitRowID := grItem(t, e, "SR-PM-1", nil)
	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)

	if _, err := e.pool.Exec(e.ctx, `DELETE FROM core.role_permission rp
		USING core.role r, core.permission p
		WHERE rp.role_id = r.id AND rp.permission_id = p.id
		  AND r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.return'`); err != nil {
		t.Fatalf("revoke permission: %v", err)
	}
	status, out, _ := soCreateReturn(t, e, supplierID, itemID, lotID, "1", nil)
	if status != http.StatusForbidden {
		t.Fatalf("create return expected 403, got %d: %v", status, out)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO core.role_permission (role_id, permission_id)
		SELECT r.id, p.id FROM core.role r, core.permission p
		WHERE r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.return'
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("restore permission: %v", err)
	}
}

// TestSupplierReturnInsufficient — returning more than the available total is
// 409 and writes nothing.
func TestSupplierReturnInsufficient(t *testing.T) {
	e := grSetup(t, "sretshort", "081234620105")
	supplierID := grCreateSupplier(t, e, "SUP-SR-5")
	itemID, unitRowID := grItem(t, e, "SR-IS-1", nil)
	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)

	status, out, id := soCreateReturn(t, e, supplierID, itemID, lotID, "100", nil)
	if status != http.StatusCreated {
		t.Fatalf("draft expected 201, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/supplier-returns/"+id+"/post", nil)
	if status != http.StatusConflict || grErr(out) != "insufficient stock" {
		t.Fatalf("post expected 409 insufficient stock, got %d: %v", status, out)
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "10.000" {
		t.Fatalf("balance = %q, want untouched 10.000", got)
	}
}

// soDestructionDraft is a valid one-line destruction draft (2 witnesses).
func soDestructionDraft(e *grEnv, itemID, lotID string, extra map[string]any) map[string]any {
	body := map[string]any{
		"location_id": e.locationID, "occurred_on": grToday(0), "method": "insinerasi",
		"witnesses": []string{"dr. Andi Wijaya", "Siti Rahayu, S.Farm"},
		"lines": []map[string]any{{
			"item_id": itemID, "lot_id": lotID, "source": "purchase",
			"from_status": "available", "base_qty": "2",
		}},
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// soCreateDestruction creates a destruction draft; returns (status, body, id).
func soCreateDestruction(t *testing.T, e *grEnv, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/destructions", body)
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id, _ = data["id"].(string)
	}
	return status, out, id
}

// TestDestructionNarcoticsWitnessRequired — a narkotika item without the
// two-witness report 400s on post; with 2 witnesses + method it posts and the
// from_status balance decreases.
func TestDestructionNarcoticsWitnessRequired(t *testing.T) {
	e := grSetup(t, "dstrnarko", "081234620106")
	supplierID := grCreateSupplier(t, e, "SUP-DS-1")
	itemID, unitRowID := grItem(t, e, "SR-NK-1", map[string]any{"drug_class": "narkotika"})
	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)

	status, out, id := soCreateDestruction(t, e, soDestructionDraft(e, itemID, lotID, map[string]any{
		"witnesses": []string{},
	}))
	if status != http.StatusCreated {
		t.Fatalf("draft expected 201, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/destructions/"+id+"/post", nil)
	if status != http.StatusBadRequest || grErr(out) != "destruction report required" {
		t.Fatalf("post expected 400 destruction report required, got %d: %v", status, out)
	}
	// Fix the report on the same draft (full replace).
	patch := soDestructionDraft(e, itemID, lotID, nil)
	patch["row_version"] = 1
	status, out = grRequest(t, e, http.MethodPatch, "/api/v1/destructions/"+id, patch)
	if status != http.StatusOK {
		t.Fatalf("patch expected 200, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/destructions/"+id+"/post", nil)
	if status != http.StatusCreated || out["data"].(map[string]any)["status"] != "posted" {
		t.Fatalf("post expected 201 posted, got %d: %v", status, out)
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "8.000" {
		t.Fatalf("available balance = %q, want 8.000", got)
	}
}

// TestDestructionCostNotChanged — a destruction move values the stock at
// avg_cost but never rewrites item_cost (spec §9.3).
func TestDestructionCostNotChanged(t *testing.T) {
	e := grSetup(t, "dstrcost", "081234620107")
	supplierID := grCreateSupplier(t, e, "SUP-DS-2")
	itemID, unitRowID := grItem(t, e, "SR-DC-1", nil)
	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)
	before := grAvgCost(t, e, itemID)

	status, out, id := soCreateDestruction(t, e, soDestructionDraft(e, itemID, lotID, nil))
	if status != http.StatusCreated {
		t.Fatalf("draft expected 201, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/destructions/"+id+"/post", nil)
	if status != http.StatusCreated {
		t.Fatalf("post expected 201, got %d: %v", status, out)
	}
	if got := grAvgCost(t, e, itemID); got != before {
		t.Fatalf("avg_cost changed %q → %q", before, got)
	}
}

// TestDestructionVoid — voiding restores the balance, keeps the HPP untouched
// and preserves posted_at (NOT NULL also after void).
func TestDestructionVoid(t *testing.T) {
	e := grSetup(t, "dstrvoid", "081234620108")
	supplierID := grCreateSupplier(t, e, "SUP-DS-3")
	itemID, unitRowID := grItem(t, e, "SR-DV-1", nil)
	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)
	before := grAvgCost(t, e, itemID)
	_, _, id := soCreateDestruction(t, e, soDestructionDraft(e, itemID, lotID, nil))
	if status, out := grRequest(t, e, http.MethodPost, "/api/v1/destructions/"+id+"/post", nil); status != http.StatusCreated {
		t.Fatalf("post expected 201, got %d: %v", status, out)
	}
	// Void without a reason is 400 with the exact contract message.
	statusNoReason, outNoReason := grRequest(t, e, http.MethodPost, "/api/v1/destructions/"+id+"/void", map[string]any{})
	if statusNoReason != http.StatusBadRequest || grErr(outNoReason) != "void reason required" {
		t.Fatalf("void without reason expected 400 void reason required, got %d: %v", statusNoReason, outNoReason)
	}
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/destructions/"+id+"/void",
		map[string]any{"reason": "salah input"})
	if status != http.StatusOK || out["data"].(map[string]any)["status"] != "void" {
		t.Fatalf("void expected 200 void, got %d: %v", status, out)
	}
	if out["data"].(map[string]any)["posted_at"] == nil {
		t.Fatalf("posted_at lost on void: %v", out["data"])
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "10.000" {
		t.Fatalf("balance after void = %q, want 10.000", got)
	}
	if got := grAvgCost(t, e, itemID); got != before {
		t.Fatalf("avg_cost changed on void: %q → %q", before, got)
	}
}

// TestDestructionForbidden — without inventory.stock.destroy creation is 403.
func TestDestructionForbidden(t *testing.T) {
	e := grSetup(t, "dstrperm", "081234620109")
	supplierID := grCreateSupplier(t, e, "SUP-DS-4")
	itemID, unitRowID := grItem(t, e, "SR-DP-1", nil)
	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)

	if _, err := e.pool.Exec(e.ctx, `DELETE FROM core.role_permission rp
		USING core.role r, core.permission p
		WHERE rp.role_id = r.id AND rp.permission_id = p.id
		  AND r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.destroy'`); err != nil {
		t.Fatalf("revoke permission: %v", err)
	}
	status, out, _ := soCreateDestruction(t, e, soDestructionDraft(e, itemID, lotID, nil))
	if status != http.StatusForbidden {
		t.Fatalf("create destruction expected 403, got %d: %v", status, out)
	}
	if _, err := e.pool.Exec(e.ctx, `INSERT INTO core.role_permission (role_id, permission_id)
		SELECT r.id, p.id FROM core.role r, core.permission p
		WHERE r.name = 'Owner' AND r.is_system AND p.code = 'inventory.stock.destroy'
		ON CONFLICT DO NOTHING`); err != nil {
		t.Fatalf("restore permission: %v", err)
	}
}

// soIMTSeed seeds company + two merchants (A sender, B receiver), warehouse
// rooms in both, one item and a 10 @ 100 purchase receipt at A. Returns the
// shared grEnv (active merchant A), merchant B, both locations, one item+lot
// and the owner's refresh cookie (for creating further merchants).
func soIMTSeed(t *testing.T, label, phone string) (*grEnv, string, string, string, string, *http.Cookie) {
	t.Helper()
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantA, _, token, cookie := seedAccountOwner(t, ctx, pool, router, "sr."+label, phone)
	merchantB := createTenantMerchant(t, ctx, pool, router, token, cookie, companyID, "Cabang SR "+label)
	e := &grEnv{pool: pool, router: router, ctx: ctx, companyID: companyID,
		merchantID: merchantA, token: token}

	tx := runtimeTenantTx(t, ctx, pool, companyID, merchantA)
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room',$3,'Gudang A',ARRAY['warehouse']::text[])
		RETURNING id::text`, companyID, merchantA, "LOC-IMT-A-"+label).Scan(&e.locationID); err != nil {
		t.Fatalf("insert location A: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit location A: %v", err)
	}
	var locB string
	tx = runtimeTenantTx(t, ctx, pool, companyID, merchantB)
	if err := tx.QueryRow(ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room',$3,'Gudang B',ARRAY['warehouse']::text[])
		RETURNING id::text`, companyID, merchantB, "LOC-IMT-B-"+label).Scan(&locB); err != nil {
		t.Fatalf("insert location B: %v", err)
	}
	if err := tx.Commit(ctx); err != nil {
		t.Fatalf("commit location B: %v", err)
	}

	supplierID := grCreateSupplier(t, e, "SUP-IMT-"+label)
	itemID, unitRowID := grItem(t, e, "SR-IMT-"+label, nil)
	receipt := grPostAndAssert(t, e, grDraftBody(e, supplierID, itemID, unitRowID, nil))
	lotID := receipt["lines"].([]any)[0].(map[string]any)["lot_id"].(string)
	return e, merchantB, locB, itemID, lotID, cookie
}

// soSendIMT ships qty base units of the lot from A to B.
func soSendIMT(t *testing.T, e *grEnv, merchantB, locB, itemID, lotID, qty string) (int, map[string]any, string) {
	t.Helper()
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/inter-merchant-transfers",
		map[string]any{
			"to_merchant_id": merchantB, "from_location_id": e.locationID, "to_location_id": locB,
			"lines": []map[string]any{{
				"item_id": itemID, "lot_id": lotID, "source": "purchase",
				"status": "available", "base_qty": qty,
			}},
		})
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id, _ = data["id"].(string)
	}
	return status, out, id
}

// TestInterMerchantTransferLifecycle — send 5 (sender −5, doc sent, receiver
// 0), receive (receiver +5, receiver avg = shipped cost, status received).
func TestInterMerchantTransferLifecycle(t *testing.T) {
	e, merchantB, locB, itemID, lotID, cookie := soIMTSeed(t, "life", "081234620110")
	tokenB := soTokenFor(t, e, cookie, merchantB)

	status, out, id := soSendIMT(t, e, merchantB, locB, itemID, lotID, "5")
	if status != http.StatusCreated || id == "" {
		t.Fatalf("send expected 201, got %d: %v", status, out)
	}
	doc := out["data"].(map[string]any)
	if doc["status"] != "sent" || !strings.HasPrefix(doc["imt_no"].(string), "IMT-") {
		t.Fatalf("sent doc = %v", doc)
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "5.000" {
		t.Fatalf("sender balance = %q, want 5.000", got)
	}
	if got := soBalanceQty(t, e, merchantB, itemID, "purchase"); got != "" {
		t.Fatalf("receiver balance before receive = %q, want absent", got)
	}

	// Receiver reads the document under merchant B.
	status, out = soRequestAs(t, e, tokenB, merchantB, http.MethodGet, "/api/v1/inter-merchant-transfers/"+id, nil)
	if status != http.StatusOK {
		t.Fatalf("get as receiver expected 200, got %d: %v", status, out)
	}
	// Receive under merchant B.
	status, out = soRequestAs(t, e, tokenB, merchantB, http.MethodPost, "/api/v1/inter-merchant-transfers/"+id+"/receive", nil)
	if status != http.StatusOK {
		t.Fatalf("receive expected 200, got %d: %v", status, out)
	}
	received := out["data"].(map[string]any)
	if received["status"] != "received" || received["received_on"] == nil || received["received_at"] == nil {
		t.Fatalf("received doc = %v", received)
	}
	if got := soBalanceQty(t, e, merchantB, itemID, "purchase"); got != "5.000" {
		t.Fatalf("receiver balance = %q, want 5.000", got)
	}
	// Receiver's avg cost = the shipped unit cost for purchase stock; the
	// seed receipt is 10 packs @ 10000 → unit_cost kirim = 10000.000000.
	if got := soAvgCost(t, e, merchantB, itemID); got != "10000.000000" {
		t.Fatalf("receiver avg = %q, want 10000.000000", got)
	}
	// In-transit report: the document no longer lists as sent.
	status, out = grRequest(t, e, http.MethodGet, "/api/v1/merchants/"+e.merchantID+"/inter-merchant-transfers?status=sent", nil)
	if status != http.StatusOK || len(out["data"].([]any)) != 0 {
		t.Fatalf("in-transit list = %d %v", status, out["data"])
	}
}

// TestInterMerchantTransferCancel — cancelling an in-transit shipment returns
// the stock and re-applies the sender's HPP.
func TestInterMerchantTransferCancel(t *testing.T) {
	e, merchantB, locB, itemID, lotID, cookie := soIMTSeed(t, "cancel", "081234620111")
	tokenB := soTokenFor(t, e, cookie, merchantB)
	before := grAvgCost(t, e, itemID)

	status, out, id := soSendIMT(t, e, merchantB, locB, itemID, lotID, "5")
	if status != http.StatusCreated {
		t.Fatalf("send expected 201, got %d: %v", status, out)
	}
	// Cancel runs under a sender session whose token carries mid = merchant A
	// (the guard checks the active merchant, not just the role).
	tokenA := soTokenFor(t, e, cookie, e.merchantID)
	status, out = soRequestAs(t, e, tokenA, e.merchantID, http.MethodPost, "/api/v1/inter-merchant-transfers/"+id+"/cancel", nil)
	if status != http.StatusOK || out["data"].(map[string]any)["status"] != "cancelled" {
		t.Fatalf("cancel expected 200 cancelled, got %d: %v", status, out)
	}
	if got := grBalanceQty(t, e, itemID, "purchase"); got != "10.000" {
		t.Fatalf("sender balance after cancel = %q, want 10.000", got)
	}
	if got := grAvgCost(t, e, itemID); got != before {
		t.Fatalf("sender avg changed %q → %q", before, got)
	}
	// Cancel is only for the sender: the receiver cannot cancel.
	status, out2, id2 := soSendIMT(t, e, merchantB, locB, itemID, lotID, "1")
	if status != http.StatusCreated {
		t.Fatalf("send 2 expected 201, got %d: %v", status, out2)
	}
	status, out2 = soRequestAs(t, e, tokenB, merchantB, http.MethodPost, "/api/v1/inter-merchant-transfers/"+id2+"/cancel", nil)
	if status != http.StatusForbidden {
		t.Fatalf("receiver cancel expected 403, got %d: %v", status, out2)
	}
}

// TestInterMerchantTransferReceiverGuard — receiving under a merchant that is
// not to_merchant_id is 403 "wrong receiving merchant".
func TestInterMerchantTransferReceiverGuard(t *testing.T) {
	e, merchantB, locB, itemID, lotID, cookie := soIMTSeed(t, "guard", "081234620112")
	merchantC := createTenantMerchant(t, e.ctx, e.pool, e.router, e.token, cookie, e.companyID, "Cabang SR C")
	tokenC := soTokenFor(t, e, cookie, merchantC)

	status, out, id := soSendIMT(t, e, merchantB, locB, itemID, lotID, "5")
	if status != http.StatusCreated {
		t.Fatalf("send expected 201, got %d: %v", status, out)
	}
	status, out = soRequestAs(t, e, tokenC, merchantC, http.MethodPost, "/api/v1/inter-merchant-transfers/"+id+"/receive", nil)
	if status != http.StatusForbidden || grErr(out) != "wrong receiving merchant" {
		t.Fatalf("receive as C expected 403 wrong receiving merchant, got %d: %v", status, out)
	}
}

// TestOutboundDailyNumbers — SRET-/DSTR- are unique per merchant per day and
// IMT- per company (two shipments from the same company share one sequence).
func TestOutboundDailyNumbers(t *testing.T) {
	e, merchantB, locB, itemID, lotID, _ := soIMTSeed(t, "numbers", "081234620113")
	supplierID := grCreateSupplier(t, e, "SUP-SR-NUM")

	_, out1, _ := soCreateReturn(t, e, supplierID, itemID, lotID, "1", nil)
	_, out2, _ := soCreateReturn(t, e, supplierID, itemID, lotID, "1", nil)
	no1 := out1["data"].(map[string]any)["return_no"].(string)
	no2 := out2["data"].(map[string]any)["return_no"].(string)
	if !strings.HasPrefix(no1, "SRET-") || !strings.HasSuffix(no1, "-0001") ||
		!strings.HasSuffix(no2, "-0002") {
		t.Fatalf("return numbers = %s, %s", no1, no2)
	}

	_, out3, _ := soCreateDestruction(t, e, soDestructionDraft(e, itemID, lotID, map[string]any{
		"lines": []map[string]any{{
			"item_id": itemID, "lot_id": lotID, "source": "purchase",
			"from_status": "available", "base_qty": "1",
		}},
	}))
	_, out4, _ := soCreateDestruction(t, e, soDestructionDraft(e, itemID, lotID, map[string]any{
		"lines": []map[string]any{{
			"item_id": itemID, "lot_id": lotID, "source": "purchase",
			"from_status": "available", "base_qty": "1",
		}},
	}))
	no3 := out3["data"].(map[string]any)["destruction_no"].(string)
	no4 := out4["data"].(map[string]any)["destruction_no"].(string)
	if !strings.HasPrefix(no3, "DSTR-") || !strings.HasSuffix(no3, "-0001") ||
		!strings.HasSuffix(no4, "-0002") {
		t.Fatalf("destruction numbers = %s, %s", no3, no4)
	}

	// IMT numbering is company-scoped: two sends from merchant A get 0001/0002.
	_, out5, _ := soSendIMT(t, e, merchantB, locB, itemID, lotID, "1")
	_, out6, _ := soSendIMT(t, e, merchantB, locB, itemID, lotID, "1")
	if out5["data"] == nil || out6["data"] == nil {
		t.Fatalf("send failed: %v %v", out5, out6)
	}
	no5 := out5["data"].(map[string]any)["imt_no"].(string)
	no6 := out6["data"].(map[string]any)["imt_no"].(string)
	if !strings.HasPrefix(no5, "IMT-") || !strings.HasSuffix(no5, "-0001") ||
		!strings.HasSuffix(no6, "-0002") || no5 == no6 {
		t.Fatalf("imt numbers = %s, %s", no5, no6)
	}
}
