//go:build integration

package server_test

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestPurchase*/TestContractPrice*/TestProcurement* cover the procurement
// API (plan 2026-10-20-inventory-17b-p1-procurement-core Tasks 2–3, spec
// 2026-10-20-inventory-17b-procurement-special-cases §4.2–4.3): the PR
// lifecycle with its permission gates, PO drafts with central-mode gating
// and contract-price prefill, send/close/cancel guards, goods-receipt ↔
// purchase-order linkage on post and void, daily document numbers and the
// contract-price upsert rules.

// poRequest sends a request with an explicit merchant context + token.
func poRequest(t *testing.T, e *grEnv, merchantID, token, method, path string, body map[string]any) (int, map[string]any) {
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

// poSeedBranch creates a second merchant of the same company with one room
// location (central-mode tests need a branch merchant next to the HQ).
func poSeedBranch(t *testing.T, e *grEnv, label string) (merchantID, locationID string) {
	t.Helper()
	tx := runtimeTenantTx(t, e.ctx, e.pool, e.companyID, e.merchantID)
	if err := tx.QueryRow(e.ctx, `
		INSERT INTO core.merchant (company_id, code, name)
		VALUES ($1, $2, 'Cabang '||$2) RETURNING id::text`,
		e.companyID, "BR-"+label).Scan(&merchantID); err != nil {
		t.Fatalf("insert branch merchant: %v", err)
	}
	if err := tx.Commit(e.ctx); err != nil {
		t.Fatalf("commit branch merchant: %v", err)
	}
	// The location lives under the branch's own RLS context.
	txLoc := runtimeTenantTx(t, e.ctx, e.pool, e.companyID, merchantID)
	if err := txLoc.QueryRow(e.ctx, `
		INSERT INTO core.location (company_id, merchant_id, parent_id, kind, code, name, functions)
		VALUES ($1,$2,NULL,'room',$3,'Gudang Cabang',ARRAY['warehouse']::text[])
		RETURNING id::text`, e.companyID, merchantID, "LOC-BR-"+label).Scan(&locationID); err != nil {
		t.Fatalf("insert branch location: %v", err)
	}
	if err := txLoc.Commit(e.ctx); err != nil {
		t.Fatalf("commit branch location: %v", err)
	}
	return merchantID, locationID
}

// poStaffToken seeds a staff user with exactly perms at merchantID and
// returns a legacy-format token for it (default role = the seeded grant).
func poStaffToken(t *testing.T, e *grEnv, merchantID, label string, perms ...string) string {
	t.Helper()
	userID := seedMerchantStaff(t, e.ctx, e.pool, e.companyID, merchantID, "staff."+label, "", perms...)
	token, err := auth.GenerateToken(testJWTSecret, userID, e.companyID, merchantID, "", "staff."+label, label+"-device", time.Hour)
	if err != nil {
		t.Fatalf("generate staff token: %v", err)
	}
	return token
}

// poSeedSetting writes the company procurement setting directly.
func poSeedSetting(t *testing.T, e *grEnv, mode string, hqID string) {
	t.Helper()
	if _, err := e.pool.Exec(e.ctx, `
		INSERT INTO inventory.procurement_setting (company_id, mode, headquarters_merchant_id)
		VALUES ($1, $2, NULLIF($3,'')::uuid) ON CONFLICT (company_id) DO UPDATE
		SET mode = EXCLUDED.mode, headquarters_merchant_id = EXCLUDED.headquarters_merchant_id`,
		e.companyID, mode, hqID); err != nil {
		t.Fatalf("seed procurement setting: %v", err)
	}
}

// poBody is a one/two-line PO draft body.
func poBody(supplierID, locationID string, lines []map[string]any, extra map[string]any) map[string]any {
	body := map[string]any{
		"supplier_id": supplierID, "location_id": locationID,
		"lines": lines,
	}
	for k, v := range extra {
		body[k] = v
	}
	return body
}

// poLineIn is one PO line: 10 packs × content 10 (base 10 per pack).
func poLineIn(itemID, unitID string, extra map[string]any) map[string]any {
	line := map[string]any{
		"item_id": itemID, "item_unit_id": unitID,
		"pack_qty": "10", "pack_content": "10",
	}
	for k, v := range extra {
		line[k] = v
	}
	return line
}

// poCreateAndAssert posts a PO draft and returns the created order map.
func poCreateAndAssert(t *testing.T, e *grEnv, merchantID, token string, body map[string]any, wantStatus int) map[string]any {
	t.Helper()
	status, out := poRequest(t, e, merchantID, token, http.MethodPost,
		"/api/v1/merchants/"+merchantID+"/purchase-orders", body)
	if status != wantStatus {
		t.Fatalf("create PO expected %d, got %d: %v", wantStatus, status, out)
	}
	if wantStatus != http.StatusCreated {
		return out
	}
	data, _ := out["data"].(map[string]any)
	if data["status"] != "draft" {
		t.Fatalf("new PO = %v", data)
	}
	return data
}

// poSend flips a draft to sent.
func poSend(t *testing.T, e *grEnv, poID string) {
	t.Helper()
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/purchase-orders/"+poID+"/send", nil)
	if status != http.StatusOK {
		t.Fatalf("send PO expected 200, got %d: %v", status, out)
	}
}

// poGet fetches one PO with lines.
func poGet(t *testing.T, e *grEnv, poID string) map[string]any {
	t.Helper()
	status, out := grRequest(t, e, http.MethodGet, "/api/v1/purchase-orders/"+poID, nil)
	if status != http.StatusOK {
		t.Fatalf("get PO expected 200, got %d: %v", status, out)
	}
	return out["data"].(map[string]any)
}

// poLineByID finds one PO line of a fetched order.
func poLineByID(t *testing.T, po map[string]any, lineID string) map[string]any {
	t.Helper()
	lines, _ := po["lines"].([]any)
	for _, raw := range lines {
		l := raw.(map[string]any)
		if l["id"] == lineID {
			return l
		}
	}
	t.Fatalf("PO line %s not found in %v", lineID, po["lines"])
	return nil
}

// poCreatePR runs the full draft → submitted → approved PR flow.
func poCreatePR(t *testing.T, e *grEnv, locationID, itemID, unitID string) (prID, prNo string) {
	t.Helper()
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/purchase-requests", map[string]any{
		"location_id": locationID, "needed_on": grToday(7),
		"lines": []map[string]any{{"item_id": itemID, "item_unit_id": unitID, "pack_qty": "10"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create PR expected 201, got %d: %v", status, out)
	}
	data := out["data"].(map[string]any)
	prID = data["id"].(string)
	for _, action := range []string{"submit", "approve"} {
		status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-requests/"+prID+"/"+action, nil)
		if status != http.StatusOK {
			t.Fatalf("PR %s expected 200, got %d: %v", action, status, out)
		}
	}
	return prID, data["pr_no"].(string)
}

// grLinkedBody is a PO-linked purchase receipt draft body; grLines carry the
// po line references.
func grLinkedBody(e *grEnv, supplierID, poID string, grLines []map[string]any) map[string]any {
	return map[string]any{
		"supplier_id": supplierID, "location_id": e.locationID, "source": "purchase",
		"received_on": grToday(0), "purchase_order_id": poID, "lines": grLines,
	}
}

// grLinkedLine is one PO-linked receipt line.
func grLinkedLine(itemID, unitID, poLineID, packQty string) map[string]any {
	return map[string]any{
		"item_id": itemID, "item_unit_id": unitID,
		"pack_qty": packQty, "pack_content": "10", "pack_price": "10000",
		"purchase_order_line_id": poLineID,
		"batch_no":               "B-PO", "expiry_date": grToday(700),
	}
}

// TestPurchaseRequestLifecycle — draft → submit → approve → convert yields a
// draft PO on the headquarters merchant (company in sentral mode, HQ = the
// PR merchant itself) and the PR ends converted.
func TestPurchaseRequestLifecycle(t *testing.T) {
	e := grSetup(t, "prlife", "081234630001")
	poSeedSetting(t, e, "sentral", e.merchantID)
	supplierID := grCreateSupplier(t, e, "SUP-PR-1")
	itemID, unitID := grItem(t, e, "PR-LF-1", nil)

	prID, _ := poCreatePR(t, e, e.locationID, itemID, unitID)
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/purchase-requests/"+prID+"/convert", map[string]any{
		"supplier_id": supplierID,
	})
	if status != http.StatusCreated {
		t.Fatalf("convert expected 201, got %d: %v", status, out)
	}
	po := out["data"].(map[string]any)
	if po["status"] != "draft" || po["merchant_id"] != e.merchantID {
		t.Fatalf("converted PO = %v", po)
	}
	if !strings.HasPrefix(po["po_no"].(string), "PO-") {
		t.Fatalf("po_no = %v", po["po_no"])
	}
	if po["request_id"] != prID {
		t.Fatalf("request_id = %v, want %s", po["request_id"], prID)
	}
	status, out = grRequest(t, e, http.MethodGet, "/api/v1/merchants/"+e.merchantID+"/purchase-requests", nil)
	if status != http.StatusOK {
		t.Fatalf("list PRs expected 200, got %d: %v", status, out)
	}
	var prStatus any
	for _, raw := range out["data"].([]any) {
		if r := raw.(map[string]any); r["id"] == prID {
			prStatus = r["status"]
		}
	}
	if prStatus != "converted" {
		t.Fatalf("PR status = %v, want converted", prStatus)
	}
}

// TestPurchaseRequestApproveForbidden — a staff user whose role only carries
// inventory.purchase.request gets 403 on approve.
func TestPurchaseRequestApproveForbidden(t *testing.T) {
	e := grSetup(t, "prforb", "081234630002")
	itemID, unitID := grItem(t, e, "PR-FB-1", nil)
	prID, _ := poCreatePR(t, e, e.locationID, itemID, unitID)

	token := poStaffToken(t, e, e.merchantID, "prforb", server.PermPurchaseRequest)
	status, out := poRequest(t, e, e.merchantID, token, http.MethodPost,
		"/api/v1/purchase-requests/"+prID+"/approve", nil)
	if status != http.StatusForbidden {
		t.Fatalf("approve without perm expected 403, got %d: %v", status, out)
	}
}

// TestPurchaseRequestRejectNeedsReason — rejecting with a blank reason is
// 400 "rejection reason required"; cancel works for draft and submitted but
// not for an approved request.
func TestPurchaseRequestRejectNeedsReason(t *testing.T) {
	e := grSetup(t, "prguard", "081234630003")
	itemID, unitID := grItem(t, e, "PR-GD-1", nil)

	// Blank rejection reason on a submitted PR, then cancel it. The PR is
	// submitted (not approved) so reject reaches the reason check and the
	// cancel guard accepts a submitted request.
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/purchase-requests", map[string]any{
		"location_id": e.locationID, "needed_on": grToday(7),
		"lines": []map[string]any{{"item_id": itemID, "item_unit_id": unitID, "pack_qty": "10"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create PR expected 201, got %d: %v", status, out)
	}
	prID := out["data"].(map[string]any)["id"].(string)
	if status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-requests/"+prID+"/submit", nil); status != http.StatusOK {
		t.Fatalf("submit expected 200, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-requests/"+prID+"/reject",
		map[string]any{"rejection_reason": "   "})
	if status != http.StatusBadRequest || grErr(out) != "rejection reason required" {
		t.Fatalf("reject expected 400 rejection reason required, got %d: %v", status, out)
	}

	// Cancel guards: submitted and draft cancel, approved does not.
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-requests/"+prID+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel submitted expected 200, got %d: %v", status, out)
	}
	status, out, draftID := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/purchase-requests", map[string]any{
		"location_id": e.locationID,
		"lines":       []map[string]any{{"item_id": itemID, "item_unit_id": unitID, "pack_qty": "5"}},
	})
	if status != http.StatusCreated {
		t.Fatalf("create draft PR expected 201, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-requests/"+draftID+"/cancel", nil)
	if status != http.StatusOK {
		t.Fatalf("cancel draft expected 200, got %d: %v", status, out)
	}
	approvedID, _ := poCreatePR(t, e, e.locationID, itemID, unitID)
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-requests/"+approvedID+"/cancel", nil)
	if status != http.StatusConflict {
		t.Fatalf("cancel approved expected 409, got %d: %v", status, out)
	}
}

// grRequest2 is grRequest returning the id too (helper above has no triple
// form; kept local to this file to avoid touching the goods-receipt file).
func grRequest2(t *testing.T, e *grEnv, method, path string, body map[string]any) (int, map[string]any, string) {
	t.Helper()
	status, out := grRequest(t, e, method, path, body)
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id, _ = data["id"].(string)
	}
	return status, out, id
}

// poCreateSupplierCompany creates a company-level supplier visible to every
// merchant of the company (branch + HQ PO tests share it).
func poCreateSupplierCompany(t *testing.T, e *grEnv, code string) string {
	t.Helper()
	status, out := grRequest(t, e, http.MethodPost, "/api/v1/suppliers", map[string]any{
		"code": code, "name": "Supplier " + code,
	})
	if status != http.StatusCreated {
		t.Fatalf("create company supplier expected 201, got %d: %v", status, out)
	}
	return out["data"].(map[string]any)["id"].(string)
}

// TestPurchaseOrderCentralMode — company in sentral mode with HQ = merchant
// A: a PO from the branch merchant is 409, from the HQ merchant 201.
func TestPurchaseOrderCentralMode(t *testing.T) {
	e := grSetup(t, "pocentral", "081234630004")
	branchID, branchLoc := poSeedBranch(t, e, "poctr")
	poSeedSetting(t, e, "sentral", e.merchantID)
	supplierID := poCreateSupplierCompany(t, e, "SUP-PO-1")
	itemID, unitID := grItem(t, e, "PO-CM-1", nil)

	staff := poStaffToken(t, e, branchID, "pocentral", server.PermPurchaseManage)
	branchBody := poBody(supplierID, branchLoc, []map[string]any{poLineIn(itemID, unitID, nil)}, nil)
	poCreateAndAssert(t, e, branchID, staff, branchBody, http.StatusConflict)
	_, out := poRequest(t, e, branchID, staff, http.MethodPost,
		"/api/v1/merchants/"+branchID+"/purchase-orders", branchBody)
	if grErr(out) != "central procurement required" {
		t.Fatalf("branch PO error = %v", grErr(out))
	}

	hqBody := poBody(supplierID, e.locationID, []map[string]any{poLineIn(itemID, unitID, nil)}, nil)
	poCreateAndAssert(t, e, e.merchantID, e.token, hqBody, http.StatusCreated)
}

// TestPurchaseOrderHybridCentralItem — hibrid mode: a PO containing a
// centrally-listed item is 409, a PO without one is 201 (branch merchant).
func TestPurchaseOrderHybridCentralItem(t *testing.T) {
	e := grSetup(t, "pohybrid", "081234630005")
	branchID, branchLoc := poSeedBranch(t, e, "pohb")
	poSeedSetting(t, e, "hibrid", e.merchantID)
	supplierID := poCreateSupplierCompany(t, e, "SUP-PO-2")
	centralID, centralUnit := grItem(t, e, "PO-HB-C", nil)
	otherID, otherUnit := grItem(t, e, "PO-HB-O", nil)
	if _, err := e.pool.Exec(e.ctx,
		"INSERT INTO inventory.procurement_central_item (company_id, item_id) VALUES ($1,$2)",
		e.companyID, centralID); err != nil {
		t.Fatalf("seed central item: %v", err)
	}

	staff := poStaffToken(t, e, branchID, "pohybrid", server.PermPurchaseManage)
	central := poBody(supplierID, branchLoc, []map[string]any{poLineIn(centralID, centralUnit, nil)}, nil)
	status, out := poRequest(t, e, branchID, staff, http.MethodPost,
		"/api/v1/merchants/"+branchID+"/purchase-orders", central)
	if status != http.StatusConflict || grErr(out) != "central procurement required" {
		t.Fatalf("central item PO expected 409, got %d: %v", status, out)
	}
	other := poBody(supplierID, branchLoc, []map[string]any{poLineIn(otherID, otherUnit, nil)}, nil)
	poCreateAndAssert(t, e, branchID, staff, other, http.StatusCreated)
}

// TestPurchaseOrderContractPriceDefault — a PO line without pack_price is
// prefilled from the effective contract (15250.00); the newest valid_from
// wins (200.00 over 100.00).
func TestPurchaseOrderContractPriceDefault(t *testing.T) {
	e := grSetup(t, "pocprice", "081234630006")
	supplierID := grCreateSupplier(t, e, "SUP-PO-3")
	itemID, unitID := grItem(t, e, "PO-CP-1", nil)

	put := func(price, validFrom string) (int, map[string]any) {
		return grRequest(t, e, http.MethodPut, "/api/v1/contract-prices", map[string]any{
			"supplier_id": supplierID, "item_id": itemID, "item_unit_id": unitID,
			"kind": "purchase", "pack_price": price, "valid_from": validFrom,
		})
	}
	// Older row first, then today's — the effective price is 200.00.
	if status, out := put("100.00", grToday(-5)); status != http.StatusCreated {
		t.Fatalf("put old contract expected 201, got %d: %v", status, out)
	}
	if status, out := put("200.00", grToday(0)); status != http.StatusCreated {
		t.Fatalf("put new contract expected 201, got %d: %v", status, out)
	}

	body := poBody(supplierID, e.locationID, []map[string]any{poLineIn(itemID, unitID, nil)}, nil)
	po := poCreateAndAssert(t, e, e.merchantID, e.token, body, http.StatusCreated)
	lines, _ := po["lines"].([]any)
	line := lines[0].(map[string]any)
	if line["pack_price"] != "200.00" {
		t.Fatalf("prefilled pack_price = %v, want 200.00", line["pack_price"])
	}
}

// TestPurchaseOrderCloseCancelGuards — close with a linked draft receipt is
// 409 "order has open receipts"; cancelling a sent order with received qty
// is 409 "order cannot be cancelled"; a draft cancels fine.
func TestPurchaseOrderCloseCancelGuards(t *testing.T) {
	e := grSetup(t, "pocguard", "081234630007")
	supplierID := grCreateSupplier(t, e, "SUP-PO-4")
	itemID, unitID := grItem(t, e, "PO-CG-1", nil)

	// PO + linked draft receipt → close blocked.
	body := poBody(supplierID, e.locationID, []map[string]any{poLineIn(itemID, unitID, nil)}, nil)
	po := poCreateAndAssert(t, e, e.merchantID, e.token, body, http.StatusCreated)
	poID := po["id"].(string)
	poLineID := po["lines"].([]any)[0].(map[string]any)["id"].(string)
	poSend(t, e, poID)
	status, out, _ := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/goods-receipts",
		grLinkedBody(e, supplierID, poID, []map[string]any{grLinkedLine(itemID, unitID, poLineID, "1")}))
	if status != http.StatusCreated {
		t.Fatalf("create linked draft expected 201, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-orders/"+poID+"/close", nil)
	if status != http.StatusConflict || grErr(out) != "order has open receipts" {
		t.Fatalf("close expected 409 order has open receipts, got %d: %v", status, out)
	}

	// Sent order with received qty > 0 cannot be cancelled (guard lives in
	// MarkPurchaseOrderCancelled; the qty is written directly so the status
	// stays 'sent').
	_, err := e.pool.Exec(e.ctx, `
		UPDATE inventory.purchase_order_line SET received_base_qty = 100 WHERE id = $1`, poLineID)
	if err != nil {
		t.Fatalf("bump received qty: %v", err)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-orders/"+poID+"/cancel",
		map[string]any{"cancel_reason": "salah order"})
	if status != http.StatusConflict || grErr(out) != "order cannot be cancelled" {
		t.Fatalf("cancel sent with received expected 409, got %d: %v", status, out)
	}

	// Draft cancels with the reason.
	draft := poCreateAndAssert(t, e, e.merchantID, e.token, poBody(supplierID, e.locationID,
		[]map[string]any{poLineIn(itemID, unitID, nil)}, nil), http.StatusCreated)
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-orders/"+draft["id"].(string)+"/cancel",
		map[string]any{"cancel_reason": "batal"})
	if status != http.StatusOK {
		t.Fatalf("cancel draft expected 200, got %d: %v", status, out)
	}
}

// TestGoodsReceiptPOLinkage — receiving 5 of 10 packs on line 1 leaves the
// PO partial with received_base_qty 50.000; the second receipt completes it
// to received with line 2 at 100.000.
func TestGoodsReceiptPOLinkage(t *testing.T) {
	e := grSetup(t, "polink", "081234630008")
	supplierID := grCreateSupplier(t, e, "SUP-PO-5")
	item1, unit1 := grItem(t, e, "PO-LK-1", nil)
	item2, unit2 := grItem(t, e, "PO-LK-2", nil)

	po := poCreateAndAssert(t, e, e.merchantID, e.token, poBody(supplierID, e.locationID, []map[string]any{
		poLineIn(item1, unit1, nil), poLineIn(item2, unit2, nil),
	}, nil), http.StatusCreated)
	poID := po["id"].(string)
	lines := po["lines"].([]any)
	line1 := lines[0].(map[string]any)["id"].(string)
	line2 := lines[1].(map[string]any)["id"].(string)
	poSend(t, e, poID)

	// First receipt: half of line 1.
	status, out, gr1 := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/goods-receipts",
		grLinkedBody(e, supplierID, poID, []map[string]any{grLinkedLine(item1, unit1, line1, "5")}))
	if status != http.StatusCreated {
		t.Fatalf("GR1 expected 201, got %d: %v", status, out)
	}
	var dbgHdr, dbgLn *string
	if err := e.pool.QueryRow(e.ctx, `
		SELECT g.purchase_order_id::text, (SELECT purchase_order_line_id::text FROM inventory.goods_receipt_line WHERE receipt_id = g.id LIMIT 1)
		FROM inventory.goods_receipt g WHERE g.id = $1`, gr1).Scan(&dbgHdr, &dbgLn); err != nil {
		t.Fatalf("dbg create read: %v", err)
	}
	t.Logf("DBG after create hdr=%v line=%v", dbgHdr, dbgLn)
	if status, out = grPost(t, e, gr1); status != http.StatusOK {
		t.Fatalf("post GR1 expected 200, got %d: %v", status, out)
	}
	got := poGet(t, e, poID)
	if got["status"] != "partial" {
		t.Fatalf("PO after GR1 = %v, want partial", got["status"])
	}
	if l := poLineByID(t, got, line1); l["received_base_qty"] != "50.000" {
		t.Fatalf("line1 received = %v, want 50.000", l["received_base_qty"])
	}

	// Second receipt: the rest of both lines.
	status, out, gr2 := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/goods-receipts",
		grLinkedBody(e, supplierID, poID, []map[string]any{
			grLinkedLine(item1, unit1, line1, "5"), grLinkedLine(item2, unit2, line2, "10"),
		}))
	if status != http.StatusCreated {
		t.Fatalf("GR2 expected 201, got %d: %v", status, out)
	}
	if status, out = grPost(t, e, gr2); status != http.StatusOK {
		t.Fatalf("post GR2 expected 200, got %d: %v", status, out)
	}
	got = poGet(t, e, poID)
	if got["status"] != "received" {
		t.Fatalf("PO after GR2 = %v, want received", got["status"])
	}
	if l := poLineByID(t, got, line2); l["received_base_qty"] != "100.000" {
		t.Fatalf("line2 received = %v, want 100.000", l["received_base_qty"])
	}
}

// TestGoodsReceiptPOVoid — voiding the completing receipt walks the PO back
// to partial and reduces received_base_qty exactly.
func TestGoodsReceiptPOVoid(t *testing.T) {
	e := grSetup(t, "povoid", "081234630009")
	supplierID := grCreateSupplier(t, e, "SUP-PO-6")
	item1, unit1 := grItem(t, e, "PO-VD-1", nil)

	po := poCreateAndAssert(t, e, e.merchantID, e.token, poBody(supplierID, e.locationID,
		[]map[string]any{poLineIn(item1, unit1, nil)}, nil), http.StatusCreated)
	poID := po["id"].(string)
	poLineID := po["lines"].([]any)[0].(map[string]any)["id"].(string)
	poSend(t, e, poID)

	postLinked := func(qty string) string {
		t.Helper()
		status, out, id := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/goods-receipts",
			grLinkedBody(e, supplierID, poID, []map[string]any{grLinkedLine(item1, unit1, poLineID, qty)}))
		if status != http.StatusCreated {
			t.Fatalf("GR expected 201, got %d: %v", status, out)
		}
		if status, out = grPost(t, e, id); status != http.StatusOK {
			t.Fatalf("post GR expected 200, got %d: %v", status, out)
		}
		return id
	}
	postLinked("4")
	gr2 := postLinked("6")
	if got := poGet(t, e, poID); got["status"] != "received" {
		t.Fatalf("PO before void = %v, want received", got["status"])
	}

	status, out := grRequest(t, e, http.MethodPost, "/api/v1/goods-receipts/"+gr2+"/void",
		map[string]any{"reason": "salah kirim"})
	if status != http.StatusOK {
		t.Fatalf("void GR2 expected 200, got %d: %v", status, out)
	}
	got := poGet(t, e, poID)
	if got["status"] != "partial" {
		t.Fatalf("PO after void = %v, want partial", got["status"])
	}
	// Contract math: 100 − 60 = 40 exactly.
	if l := poLineByID(t, got, poLineID); l["received_base_qty"] != "40.000" {
		t.Fatalf("received after void = %v, want 40.000", l["received_base_qty"])
	}
}

// TestGoodsReceiptPOClosedStays — a closed order keeps its status when its
// receipt is voided (spec §4.2 #4).
func TestGoodsReceiptPOClosedStays(t *testing.T) {
	e := grSetup(t, "poclosed", "081234630010")
	supplierID := grCreateSupplier(t, e, "SUP-PO-7")
	item1, unit1 := grItem(t, e, "PO-CS-1", nil)

	po := poCreateAndAssert(t, e, e.merchantID, e.token, poBody(supplierID, e.locationID,
		[]map[string]any{poLineIn(item1, unit1, nil)}, nil), http.StatusCreated)
	poID := po["id"].(string)
	poLineID := po["lines"].([]any)[0].(map[string]any)["id"].(string)
	poSend(t, e, poID)
	status, out, grID := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/goods-receipts",
		grLinkedBody(e, supplierID, poID, []map[string]any{grLinkedLine(item1, unit1, poLineID, "4")}))
	if status != http.StatusCreated {
		t.Fatalf("GR expected 201, got %d: %v", status, out)
	}
	if status, out = grPost(t, e, grID); status != http.StatusOK {
		t.Fatalf("post GR expected 200, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/purchase-orders/"+poID+"/close", nil)
	if status != http.StatusOK {
		t.Fatalf("close expected 200, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPost, "/api/v1/goods-receipts/"+grID+"/void",
		map[string]any{"reason": "retur"})
	if status != http.StatusOK {
		t.Fatalf("void expected 200, got %d: %v", status, out)
	}
	if got := poGet(t, e, poID); got["status"] != "closed" {
		t.Fatalf("closed PO after void = %v, want closed", got["status"])
	}
}

// TestGoodsReceiptPOSupplierMismatch — a receipt against a PO of another
// supplier is 409 "receipt supplier mismatch".
func TestGoodsReceiptPOSupplierMismatch(t *testing.T) {
	e := grSetup(t, "posup", "081234630011")
	supplier1 := grCreateSupplier(t, e, "SUP-PO-8A")
	supplier2 := grCreateSupplier(t, e, "SUP-PO-8B")
	item1, unit1 := grItem(t, e, "PO-SM-1", nil)

	po := poCreateAndAssert(t, e, e.merchantID, e.token, poBody(supplier1, e.locationID,
		[]map[string]any{poLineIn(item1, unit1, nil)}, nil), http.StatusCreated)
	poID := po["id"].(string)
	poLineID := po["lines"].([]any)[0].(map[string]any)["id"].(string)
	poSend(t, e, poID)

	body := grLinkedBody(e, supplier2, poID, []map[string]any{grLinkedLine(item1, unit1, poLineID, "1")})
	status, out, _ := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/goods-receipts", body)
	if status != http.StatusConflict || grErr(out) != "receipt supplier mismatch" {
		t.Fatalf("expected 409 receipt supplier mismatch, got %d: %v", status, out)
	}
}

// TestProcurementDailyNumbers — two PRs on one day number 0001/0002; POs
// share the day but run in their own scope starting at 0001.
func TestProcurementDailyNumbers(t *testing.T) {
	e := grSetup(t, "ponum", "081234630012")
	itemID, unitID := grItem(t, e, "PO-NM-1", nil)
	today := time.Now().Format("20060102")

	prBody := map[string]any{
		"location_id": e.locationID,
		"lines":       []map[string]any{{"item_id": itemID, "item_unit_id": unitID, "pack_qty": "1"}},
	}
	_, out, _ := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/purchase-requests", prBody)
	_, out2, _ := grRequest2(t, e, http.MethodPost, "/api/v1/merchants/"+e.merchantID+"/purchase-requests", prBody)
	no1 := out["data"].(map[string]any)["pr_no"].(string)
	no2 := out2["data"].(map[string]any)["pr_no"].(string)
	if no1 != "PR-"+today+"-0001" || no2 != "PR-"+today+"-0002" {
		t.Fatalf("pr numbers = %s, %s; want PR-%s-0001/0002", no1, no2, today)
	}

	supplierID := grCreateSupplier(t, e, "SUP-PO-9")
	po := poCreateAndAssert(t, e, e.merchantID, e.token, poBody(supplierID, e.locationID,
		[]map[string]any{poLineIn(itemID, unitID, nil)}, nil), http.StatusCreated)
	if got := po["po_no"].(string); got != "PO-"+today+"-0001" {
		t.Fatalf("po_no = %s, want PO-%s-0001", got, today)
	}
}

// TestContractPriceUpsert — a duplicate (supplier, item, unit, kind,
// valid_from) is 409 without overwriting; GetEffectiveContractPrice picks
// the newest valid_from ≤ today (proven via PO prefill: 15250.00 row beats
// the older 10000.00 row).
func TestContractPriceUpsert(t *testing.T) {
	e := grSetup(t, "poup", "081234630013")
	supplierID := grCreateSupplier(t, e, "SUP-PO-10")
	itemID, unitID := grItem(t, e, "PO-UP-1", nil)

	body := map[string]any{
		"supplier_id": supplierID, "item_id": itemID, "item_unit_id": unitID,
		"kind": "purchase", "pack_price": "15250.00", "valid_from": grToday(-1),
	}
	status, out := grRequest(t, e, http.MethodPut, "/api/v1/contract-prices", body)
	if status != http.StatusCreated {
		t.Fatalf("put contract expected 201, got %d: %v", status, out)
	}
	status, out = grRequest(t, e, http.MethodPut, "/api/v1/contract-prices", body)
	if status != http.StatusConflict || grErr(out) != "contract price already exists" {
		t.Fatalf("duplicate contract expected 409, got %d: %v", status, out)
	}
	// Older row inserted afterwards never wins the effective lookup.
	old := map[string]any{
		"supplier_id": supplierID, "item_id": itemID, "item_unit_id": unitID,
		"kind": "purchase", "pack_price": "10000.00", "valid_from": grToday(-30),
	}
	if status, out = grRequest(t, e, http.MethodPut, "/api/v1/contract-prices", old); status != http.StatusCreated {
		t.Fatalf("put old contract expected 201, got %d: %v", status, out)
	}
	po := poCreateAndAssert(t, e, e.merchantID, e.token, poBody(supplierID, e.locationID,
		[]map[string]any{poLineIn(itemID, unitID, nil)}, nil), http.StatusCreated)
	lines, _ := po["lines"].([]any)
	if line := lines[0].(map[string]any); line["pack_price"] != "15250.00" {
		t.Fatalf("effective pack_price = %v, want 15250.00", line["pack_price"])
	}
}
