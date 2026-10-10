//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
)

// TestInventoryHET_* covers the hard HET ceiling on manual prices and the HET
// cap on resolve/grid results (spec 2026-10-07-tariff-c-drug-master-pricing
// §4.3).

// hetComponent creates one rate component and returns its id.
func hetComponent(t *testing.T, e *ipEnv) string {
	t.Helper()
	status, out := ipRequest(t, e, http.MethodPost, "/api/v1/rate-components", map[string]any{
		"merchant_id": e.merchantID, "code": "BARANG", "name": "Harga Barang",
	})
	return plDataID(t, status, out, http.StatusCreated)
}

// hetSetPrice sets one component price for an item in a list.
func hetSetPrice(t *testing.T, e *ipEnv, listID, itemID, compID, from, amount string) (int, map[string]any) {
	t.Helper()
	return ipRequest(t, e, http.MethodPut, "/api/v1/price-lists/"+listID+"/items/"+itemID+"/price", map[string]any{
		"effective_from": from,
		"components":     []map[string]string{{"rate_component_id": compID, "amount": amount}},
	})
}

// hetDerivedList creates a derived list with +30% on top of the base list.
func hetDerivedList(t *testing.T, e *ipEnv, code string) string {
	t.Helper()
	status, out := ipRequest(t, e, http.MethodPost, "/api/v1/price-lists", map[string]any{
		"merchant_id": e.merchantID, "code": code, "name": code + " list",
		"base_price_list_id": e.listID, "adjustment_percent": "30.00", "rounding_unit": "0.00",
	})
	return plDataID(t, status, out, http.StatusCreated)
}

// hetResolve resolves an item price on today's date and returns the data.
func hetResolve(t *testing.T, e *ipEnv, listID, itemID string, want int) map[string]any {
	t.Helper()
	status, out := ipRequest(t, e, http.MethodGet,
		"/api/v1/price-lists/"+listID+"/resolve?item_id="+itemID+"&date="+ipToday(0), nil)
	if status != want {
		t.Fatalf("resolve = %d, want %d: %v", status, want, out)
	}
	data, _ := out["data"].(map[string]any)
	return data
}

// TestInventoryHET_SetPriceRejected — a manual total above the HET is a 409
// "price exceeds HET <amount>"; a total at the HET is accepted; a HET that is
// not yet effective does not constrain today's price.
func TestInventoryHET_SetPriceRejected(t *testing.T) {
	e := ipSetup(t, "hetset", "081234620011", map[string]any{
		"het_amount": "1000", "het_effective_from": ipToday(-1),
	})
	compID := hetComponent(t, e)

	status, out := hetSetPrice(t, e, e.listID, e.serviceItemID, compID, ipToday(0), "1000.01")
	if status != http.StatusConflict || plErrorMsg(out) != "price exceeds HET 1000.00" {
		t.Fatalf("over HET = %d %v, want 409 'price exceeds HET 1000.00'", status, out)
	}
	status, out = hetSetPrice(t, e, e.listID, e.serviceItemID, compID, ipToday(0), "1000")
	if status != http.StatusOK {
		t.Fatalf("at HET = %d %v, want 200", status, out)
	}

	// A second formulary item whose HET only starts tomorrow: today's price
	// is unconstrained (the first item cannot be reused — its version for
	// today already exists, so re-setting today would hit the tariff-a
	// version rule with a 409).
	ctx := context.Background()
	ipKfaSeq++
	_, _, item2ID := invCreateItem(t, e.router, e.token, e.companyID, e.merchantID, "IP-HETSET2", map[string]any{
		"base_unit_id":       invUnitID(t, ctx, e.pool, "tablet"),
		"kfa_code":           "930009900",
		"het_amount":         "1000",
		"het_effective_from": ipToday(1),
	})
	box := invUnitID(t, ctx, e.pool, "box")
	rec := accountRequest(e.router, http.MethodPost, "/api/v1/inventory/items/"+item2ID+"/units", e.token, e.companyID, e.merchantID,
		[]byte(`{"unit_id":"`+box+`","factor":"100"}`))
	if rec.Code != http.StatusCreated {
		t.Fatalf("add box 2: %d %s", rec.Code, rec.Body.String())
	}
	_, outForm, entry2ID := formAdd(t, e.router, e.token, e.companyID, e.merchantID, item2ID)
	serviceItem2ID, _ := outForm["data"].(map[string]any)["service_item_id"].(string)
	if serviceItem2ID == "" || entry2ID == "" {
		t.Fatalf("missing second formulary entry: %v", outForm)
	}
	status, out = hetSetPrice(t, e, e.listID, serviceItem2ID, compID, ipToday(0), "1500")
	if status != http.StatusOK {
		t.Fatalf("future HET today = %d %v, want 200", status, out)
	}
}

// TestInventoryHET_DerivedCapped — a derived +30% price above the HET is
// clamped to the HET on resolve and flagged het_capped on resolve and on the
// grid; a non-drug service without a HET is never capped.
func TestInventoryHET_DerivedCapped(t *testing.T) {
	e := ipSetup(t, "hetcap", "081234620012", map[string]any{
		"het_amount": "1000", "het_effective_from": ipToday(-1),
	})
	compID := hetComponent(t, e)
	if status, out := hetSetPrice(t, e, e.listID, e.serviceItemID, compID, ipToday(0), "900"); status != http.StatusOK {
		t.Fatalf("base price: %d %v", status, out)
	}
	derivedID := hetDerivedList(t, e, "het-turunan")

	// 900 × 1.30 = 1170 → clamped to the HET 1000.
	res := hetResolve(t, e, derivedID, e.serviceItemID, http.StatusOK)
	if res["total"] != "1000.00" || res["het_capped"] != true {
		t.Fatalf("derived resolve = %v, want 1000.00 capped", res)
	}

	status, out := ipRequest(t, e, http.MethodGet, "/api/v1/price-lists/"+derivedID+"/items?date="+ipToday(0), nil)
	if status != http.StatusOK {
		t.Fatalf("grid = %d %v", status, out)
	}
	rows, _ := out["data"].([]any)
	var drugRow map[string]any
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["item_id"] == e.serviceItemID {
			drugRow = row
		}
	}
	if drugRow == nil {
		t.Fatalf("drug row missing from grid: %v", out)
	}
	if drugRow["het_capped"] != true {
		t.Fatalf("grid drug row = %v, want het_capped true", drugRow)
	}

	// A non-formulary service: no HET row → never capped.
	status, out = ipRequest(t, e, http.MethodPost, "/api/v1/service-items", map[string]any{
		"merchant_id": e.merchantID, "code": "HET-PROC", "name": "procedure tanpa HET", "item_type": "procedure",
	})
	procID := plDataID(t, status, out, http.StatusCreated)
	if status, out := hetSetPrice(t, e, e.listID, procID, compID, ipToday(0), "800"); status != http.StatusOK {
		t.Fatalf("procedure base price: %d %v", status, out)
	}
	res = hetResolve(t, e, derivedID, procID, http.StatusOK)
	// 800 × 1.30 = 1040, no HET → uncapped.
	if res["total"] != "1040.00" || res["het_capped"] != false {
		t.Fatalf("procedure resolve = %v, want 1040.00 uncapped", res)
	}
	for _, raw := range rows {
		row := raw.(map[string]any)
		if row["item_id"] == procID && row["het_capped"] != false {
			t.Fatalf("procedure grid row = %v, want het_capped false", row)
		}
	}
}
