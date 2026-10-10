//go:build integration

package server_test

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/gin-gonic/gin"

	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestInventoryFormulary_* covers the per-merchant formulary API (spec
// 2026-10-07-tariff-c-drug-master-pricing §4.1, §4.2.1): activation creates a
// core.service_item, deactivation mirrors onto it, the company item cannot be
// deactivated while active in a formulary, reference prices snapshot the
// packaging factor, and renames propagate through the DB trigger.

// formAdd posts to the merchant formulary and returns (status, body, entry id).
func formAdd(t *testing.T, router *gin.Engine, token, companyID, merchantID, itemID string) (int, map[string]any, string) {
	t.Helper()
	raw, _ := json.Marshal(map[string]any{"item_id": itemID})
	rec := accountRequest(router, http.MethodPost, "/api/v1/merchants/"+merchantID+"/formulary", token, companyID, merchantID, raw)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	id := ""
	if data, ok := out["data"].(map[string]any); ok {
		id, _ = data["id"].(string)
	}
	return rec.Code, out, id
}

// formPatch patches a formulary entry.
func formPatch(t *testing.T, router *gin.Engine, token, companyID, merchantID, entryID string, body map[string]any) (int, map[string]any) {
	t.Helper()
	raw, _ := json.Marshal(body)
	rec := accountRequest(router, http.MethodPatch, "/api/v1/formulary/"+entryID, token, companyID, merchantID, raw)
	out := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out
}

// TestInventoryFormulary_Activate — activating a drug item creates a new
// core.service_item (code = item code, item_type drug); a second activation
// → 409; a colliding service item code (created via POST /service-items) →
// 409 with the exact message.
func TestInventoryFormulary_Activate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "formact", "081234610011")
	tablet := invUnitID(t, ctx, pool, "tablet")

	_, _, itemID := invCreateItem(t, router, token, companyID, merchantID, "FRM-AC-1", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000011",
	})
	status, out, entryID := formAdd(t, router, token, companyID, merchantID, itemID)
	if status != http.StatusCreated {
		t.Fatalf("add to formulary expected 201, got %d: %v", status, out)
	}
	if entryID == "" {
		t.Fatalf("missing entry id: %v", out)
	}
	row := out["data"].(map[string]any)
	if row["is_active"] != true || row["reference"] != nil {
		t.Fatalf("new entry = %v", row)
	}
	itemRef, _ := row["item"].(map[string]any)
	if itemRef["code"] != "FRM-AC-1" || itemRef["item_kind"] != "drug" || itemRef["base_unit_code"] != "tablet" {
		t.Fatalf("entry item = %v", itemRef)
	}
	serviceItemID, _ := row["service_item_id"].(string)

	// core.service_item: code = item code, item_type drug, active.
	var siCode, siType string
	var siActive bool
	if err := pool.QueryRow(ctx, "SELECT code, item_type, is_active FROM core.service_item WHERE id = $1", serviceItemID).
		Scan(&siCode, &siType, &siActive); err != nil {
		t.Fatalf("service item lookup: %v", err)
	}
	if siCode != "FRM-AC-1" || siType != "drug" || !siActive {
		t.Fatalf("service item = %q %q %v", siCode, siType, siActive)
	}

	// Second activation of the same item → 409.
	status, out, _ = formAdd(t, router, token, companyID, merchantID, itemID)
	if status != http.StatusConflict {
		t.Fatalf("duplicate add expected 409, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "item already in formulary" {
		t.Fatalf("error = %q", msg)
	}

	// A service item with the SAME code already exists for another item →
	// the formulary activation collides with the exact message.
	_, _, item2ID := invCreateItem(t, router, token, companyID, merchantID, "FRM-AC-2", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000012",
	})
	raw, _ := json.Marshal(map[string]any{
		"merchant_id": merchantID, "code": "FRM-AC-2", "name": "Bentrok", "item_type": "drug",
	})
	rec := accountRequest(router, http.MethodPost, "/api/v1/service-items", token, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("seed service item expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	status, out, _ = formAdd(t, router, token, companyID, merchantID, item2ID)
	if status != http.StatusConflict {
		t.Fatalf("colliding add expected 409, got %d: %v", status, out)
	}
	if msg := invErr(out); msg != "service item code FRM-AC-2 already exists in this merchant" {
		t.Fatalf("error = %q", msg)
	}
}

// TestInventoryFormulary_Deactivate — PATCH is_active=false deactivates the
// service_item too; a company item that is still active in a formulary cannot
// be deactivated (DB trigger → 409 with the trigger message).
func TestInventoryFormulary_Deactivate(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "formdea", "081234610012")
	tablet := invUnitID(t, ctx, pool, "tablet")

	_, _, itemID := invCreateItem(t, router, token, companyID, merchantID, "FRM-DE-1", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000013",
	})
	_, _, entryID := formAdd(t, router, token, companyID, merchantID, itemID)

	status, out := formPatch(t, router, token, companyID, merchantID, entryID, map[string]any{"is_active": false, "row_version": 1})
	if status != http.StatusOK {
		t.Fatalf("deactivate expected 200, got %d: %v", status, out)
	}
	row := out["data"].(map[string]any)
	if row["is_active"] != false {
		t.Fatalf("patched entry = %v", row)
	}
	serviceItemID, _ := row["service_item_id"].(string)
	var siActive bool
	if err := pool.QueryRow(ctx, "SELECT is_active FROM core.service_item WHERE id = $1", serviceItemID).Scan(&siActive); err != nil {
		t.Fatalf("service item lookup: %v", err)
	}
	if siActive {
		t.Fatalf("service item still active after deactivation")
	}

	// Item 2 stays active in the formulary → company deactivation blocked.
	_, _, item2ID := invCreateItem(t, router, token, companyID, merchantID, "FRM-DE-2", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000014",
	})
	_, _, _ = formAdd(t, router, token, companyID, merchantID, item2ID)
	raw, _ := json.Marshal(map[string]any{"is_active": false, "row_version": 1})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/inventory/items/"+item2ID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusConflict {
		t.Fatalf("item deactivate expected 409, got %d: %s", rec.Code, rec.Body.String())
	}
	blocked := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &blocked)
	if msg := invErr(blocked); msg != "item is active in a formulary" {
		t.Fatalf("error = %q", msg)
	}
}

// TestInventoryFormulary_ReferencePrice — a box-100 packaging with amount
// "150000" stores factor "100.000" and base_unit_amount "1500.00"; a
// packaging unit of another item is rejected with 400.
func TestInventoryFormulary_ReferencePrice(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "formref", "081234610013")
	tablet := invUnitID(t, ctx, pool, "tablet")
	box := invUnitID(t, ctx, pool, "box")

	_, _, itemID := invCreateItem(t, router, token, companyID, merchantID, "FRM-RP-1", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000015",
	})
	// Add the box-100 packaging.
	raw, _ := json.Marshal(map[string]any{"unit_id": box, "factor": "100"})
	rec := accountRequest(router, http.MethodPost, "/api/v1/inventory/items/"+itemID+"/units", token, companyID, merchantID, raw)
	if rec.Code != http.StatusCreated {
		t.Fatalf("add box expected 201, got %d: %s", rec.Code, rec.Body.String())
	}
	status, out := invGet(t, router, token, companyID, merchantID, itemID)
	data := invDetail(t, status, out, http.StatusOK)
	var boxRowID string
	for _, u := range data["units"].([]any) {
		row := u.(map[string]any)
		if row["unit"].(map[string]any)["code"] == "box" {
			boxRowID = row["id"].(string)
		}
	}
	if boxRowID == "" {
		t.Fatalf("box packaging row missing: %v", data)
	}

	_, _, entryID := formAdd(t, router, token, companyID, merchantID, itemID)

	raw, _ = json.Marshal(map[string]any{"amount": "150000", "item_unit_id": boxRowID})
	rec = accountRequest(router, http.MethodPut, "/api/v1/formulary/"+entryID+"/reference-price", token, companyID, merchantID, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("set reference price expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	okOut := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &okOut)
	row := invDetail(t, http.StatusOK, okOut, http.StatusOK)
	ref, _ := row["reference"].(map[string]any)
	if ref == nil {
		t.Fatalf("reference missing: %v", row)
	}
	// 150000.00 ÷ 100.000 = 1500.00 per base unit.
	if ref["amount"] != "150000.00" || ref["factor"] != "100.000" || ref["base_unit_amount"] != "1500.00" {
		t.Fatalf("reference = %v", ref)
	}
	if ref["unit_code"] != "box" || ref["updated_at"] == nil {
		t.Fatalf("reference meta = %v", ref)
	}

	// A packaging unit of ANOTHER item → 400 invalid packaging unit.
	_, _, item2ID := invCreateItem(t, router, token, companyID, merchantID, "FRM-RP-2", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000016",
	})
	status, out2 := invGet(t, router, token, companyID, merchantID, item2ID)
	data2 := invDetail(t, status, out2, http.StatusOK)
	otherUnits, _ := data2["units"].([]any)
	otherUnitID := otherUnits[0].(map[string]any)["id"].(string)
	raw, _ = json.Marshal(map[string]any{"amount": "150000", "item_unit_id": otherUnitID})
	rec = accountRequest(router, http.MethodPut, "/api/v1/formulary/"+entryID+"/reference-price", token, companyID, merchantID, raw)
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("foreign packaging expected 400, got %d: %s", rec.Code, rec.Body.String())
	}
	bad := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &bad)
	if msg := invErr(bad); msg != "invalid packaging unit" {
		t.Fatalf("error = %q", msg)
	}
}

// TestInventoryFormulary_RenamePropagates — renaming the company item
// propagates to the merchant's core.service_item (SECURITY DEFINER trigger).
func TestInventoryFormulary_RenamePropagates(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())

	companyID, merchantID, token := invSeed(t, ctx, pool, router, "formname", "081234610014")
	tablet := invUnitID(t, ctx, pool, "tablet")

	_, _, itemID := invCreateItem(t, router, token, companyID, merchantID, "FRM-RN-1", map[string]any{
		"base_unit_id": tablet, "kfa_code": "930000017",
	})
	_, out, _ := formAdd(t, router, token, companyID, merchantID, itemID)
	serviceItemID := out["data"].(map[string]any)["service_item_id"].(string)

	raw, _ := json.Marshal(map[string]any{"name": "Paracetamol Baru", "row_version": 1})
	rec := accountRequest(router, http.MethodPatch, "/api/v1/inventory/items/"+itemID, token, companyID, merchantID, raw)
	if rec.Code != http.StatusOK {
		t.Fatalf("rename expected 200, got %d: %s", rec.Code, rec.Body.String())
	}

	rec = accountRequest(router, http.MethodGet, "/api/v1/service-items/"+serviceItemID, token, companyID, merchantID, nil)
	if rec.Code != http.StatusOK {
		t.Fatalf("get service item expected 200, got %d: %s", rec.Code, rec.Body.String())
	}
	siOut := map[string]any{}
	_ = json.Unmarshal(rec.Body.Bytes(), &siOut)
	si := invDetail(t, http.StatusOK, siOut, http.StatusOK)
	// GET /service-items/:id returns the raw sqlcgen struct (PascalCase JSON).
	if si["Name"] != "Paracetamol Baru" {
		t.Fatalf("service item name = %v, want Paracetamol Baru", si["Name"])
	}
}
