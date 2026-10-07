//go:build integration

package server_test

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"testing"
)

// TestPriceTemplate_* covers the price-list template API (spec
// 2026-10-07-tariff-price-lists §6): listing the BPJS non-capitation template
// and applying it into a standalone price list with fixed/range/max/regional
// value kinds, validation before any write, and item reuse.

// ptTemplate fetches the price-list template list and returns the
// bpjs_nonkapitasi_2023 template view (id + items).
func ptTemplate(t *testing.T, env *qeEnv) (string, []map[string]any) {
	t.Helper()
	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/price-list-templates", nil)
	if status != http.StatusOK {
		t.Fatalf("GET price-list-templates = %d: %v", status, out)
	}
	data, _ := out["data"].([]any)
	for _, raw := range data {
		tpl := raw.(map[string]any)
		if tpl["code"] == "bpjs_nonkapitasi_2023" {
			items, _ := tpl["items"].([]any)
			views := make([]map[string]any, 0, len(items))
			for _, it := range items {
				views = append(views, it.(map[string]any))
			}
			id, _ := tpl["id"].(string)
			return id, views
		}
	}
	t.Fatalf("bpjs_nonkapitasi_2023 template not found: %v", out)
	return "", nil
}

// ptApply posts an apply request and returns status + body.
func ptApply(t *testing.T, env *qeEnv, templateID string, scope string, values map[string]string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"merchant_id":     env.merchantID,
		"price_list_code": "bpjs-" + scope + "-" + ptSeq(),
		"price_list_name": "BPJS " + scope,
		"scope":           scope,
		"effective_from":  plToday(env, 0),
		"values":          values,
	}
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/price-list-templates/"+templateID+"/apply", body)
	return status, out
}

// ptSeq yields a unique numeric suffix per call (labels stay unique across runs).
func ptSeq() string {
	return strconv.FormatInt(qePhoneSeq.Add(1), 10) + "x"
}

// ptItemCount counts the merchant's live service items with the given code.
func ptItemCount(t *testing.T, env *qeEnv, code string) int {
	t.Helper()
	var n int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.service_item WHERE merchant_id=$1 AND code=$2 AND deleted_at IS NULL",
		env.merchantID, code).Scan(&n); err != nil {
		t.Fatalf("count items %s: %v", code, err)
	}
	return n
}

// TestPriceTemplate_List — the BPJS non-capitation template is listed with 34
// items; ANC-BIDAN's fixed amount reads back as "70000.00".
func TestPriceTemplate_List(t *testing.T) {
	env := plNewEnv(t, "tplist")
	_, items := ptTemplate(t, env)
	if len(items) != 34 {
		t.Fatalf("template items = %d, want 34", len(items))
	}
	for _, it := range items {
		if it["code"] == "ANC-BIDAN" {
			if it["amount_min"] != "70000.00" || it["amount_max"] != "70000.00" {
				t.Fatalf("ANC-BIDAN amounts = %v / %v, want 70000.00", it["amount_min"], it["amount_max"])
			}
			if it["value_kind"] != "fixed" || it["item_type"] != "procedure" {
				t.Fatalf("ANC-BIDAN kinds = %v / %v", it["value_kind"], it["item_type"])
			}
			return
		}
	}
	t.Fatalf("ANC-BIDAN missing from items: %v", items)
}

// TestPriceTemplate_ApplyBidan — applying the bidan_jejaring scope creates a
// standalone list with one item + KLAIM price per scoped row; the max-kind
// value given becomes the price; the application is recorded once.
func TestPriceTemplate_ApplyBidan(t *testing.T) {
	env := plNewEnv(t, "tplbidan")
	templateID, items := ptTemplate(t, env)
	wantScoped := 0
	for _, it := range items {
		if scopes, _ := it["scopes"].([]any); len(scopes) == 2 {
			wantScoped++
		}
	}

	status, out := ptApply(t, env, templateID, "bidan_jejaring", map[string]string{"PRARUJUK-KOMPLIKASI": "150000"})
	if status != http.StatusCreated {
		t.Fatalf("apply = %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("missing data: %v", out)
	}
	pl, _ := data["price_list"].(map[string]any)
	if pl == nil || pl["id"] == "" {
		t.Fatalf("missing price_list: %v", data)
	}
	listID, _ := pl["id"].(string)

	// One service_rate per scoped row, each a single KLAIM component.
	var nRates int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.service_rate WHERE price_list_id=$1 AND deleted_at IS NULL", listID).Scan(&nRates); err != nil {
		t.Fatalf("count rates: %v", err)
	}
	if nRates != wantScoped {
		t.Fatalf("rates = %d, want %d scoped rows", nRates, wantScoped)
	}

	// Resolve the fixed ANC-BIDAN and the max-bounded pra-rujukan price.
	resolve := func(code, want string) {
		t.Helper()
		var itemID string
		if err := env.pool.QueryRow(context.Background(),
			"SELECT id::text FROM core.service_item WHERE merchant_id=$1 AND code=$2 AND deleted_at IS NULL",
			env.merchantID, code).Scan(&itemID); err != nil {
			t.Fatalf("item %s: %v", code, err)
		}
		res := plResolveData(t, env, listID, itemID, plToday(env, 0))
		if res["total"] != want {
			t.Fatalf("%s total = %v, want %s", code, res["total"], want)
		}
	}
	resolve("ANC-BIDAN", "70000.00")
	resolve("PRARUJUK-KOMPLIKASI", "150000.00")

	var nApps int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.template_application WHERE template_id=$1 AND company_id=$2",
		templateID, env.companyID).Scan(&nApps); err != nil {
		t.Fatalf("count applications: %v", err)
	}
	if nApps != 1 {
		t.Fatalf("template_application rows = %d, want 1", nApps)
	}
}

// fktpRequiredValues are the values every fktp apply must carry: the four
// range items and the regional ambulans tariff.
func fktpRequiredValues() map[string]string {
	return map[string]string{
		"SKR-GULA-DARAH": "15000",
		"RANAP-TP":       "250000",
		"PRB-GULA-DARAH": "15000",
		"PRB-HBA1C":      "180000",
		"AMBULANS":       "100000",
	}
}

// ptCountListsAndItems counts the merchant's price lists and service items —
// the two writes a failed apply must never leave behind.
func ptCountListsAndItems(t *testing.T, env *qeEnv) (int, int) {
	t.Helper()
	var nLists, nItems int
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.price_list WHERE merchant_id=$1 AND deleted_at IS NULL", env.merchantID).Scan(&nLists); err != nil {
		t.Fatalf("count lists: %v", err)
	}
	if err := env.pool.QueryRow(context.Background(),
		"SELECT count(*) FROM core.service_item WHERE merchant_id=$1 AND deleted_at IS NULL", env.merchantID).Scan(&nItems); err != nil {
		t.Fatalf("count items: %v", err)
	}
	return nLists, nItems
}

// TestPriceTemplate_ApplyFktpValidation — every rejected apply (missing or
// out-of-range value, empty regional, changed fixed) leaves no price list and
// no service item behind.
func TestPriceTemplate_ApplyFktpValidation(t *testing.T) {
	env := plNewEnv(t, "tplfktp")
	templateID, _ := ptTemplate(t, env)

	assertRejected := func(label string, values map[string]string, wantContains string) {
		t.Helper()
		beforeLists, beforeItems := ptCountListsAndItems(t, env)
		status, out := ptApply(t, env, templateID, "fktp", values)
		if status != http.StatusBadRequest {
			t.Fatalf("%s = %d %v, want 400", label, status, out)
		}
		if wantContains != "" && !strings.Contains(plErrorMsg(out), wantContains) {
			t.Fatalf("%s error = %q, want contains %q", label, plErrorMsg(out), wantContains)
		}
		afterLists, afterItems := ptCountListsAndItems(t, env)
		if afterLists != beforeLists || afterItems != beforeItems {
			t.Fatalf("%s wrote rows: lists %d→%d items %d→%d", label, beforeLists, afterLists, beforeItems, afterItems)
		}
	}

	// Missing range value SKR-GULA-DARAH.
	values := fktpRequiredValues()
	delete(values, "SKR-GULA-DARAH")
	assertRejected("missing range", values, "SKR-GULA-DARAH")

	// Range value out of bounds: 25000 > 20000.
	values = fktpRequiredValues()
	values["SKR-GULA-DARAH"] = "25000"
	assertRejected("range above max", values, "SKR-GULA-DARAH")

	// Empty regional value.
	values = fktpRequiredValues()
	values["AMBULANS"] = ""
	assertRejected("empty regional", values, "AMBULANS")

	// Fixed tariff changed: ANC-DR is fixed at 90000.
	values = fktpRequiredValues()
	values["ANC-DR"] = "95000"
	assertRejected("fixed changed", values, "ANC-DR")

	// Past effective_from is rejected with no price list written.
	beforeLists, _ := ptCountListsAndItems(t, env)
	body := map[string]any{
		"merchant_id":     env.merchantID,
		"price_list_code": "bpjs-past-" + ptSeq(),
		"price_list_name": "BPJS fktp past",
		"scope":           "fktp",
		"effective_from":  plToday(env, -1),
		"values":          fktpRequiredValues(),
	}
	status, out := qeRequest(t, env, http.MethodPost, "/api/v1/price-list-templates/"+templateID+"/apply", body)
	if status != http.StatusConflict || plErrorMsg(out) != "effective_from cannot be in the past" {
		t.Fatalf("past effective_from = %d %v, want 409 'effective_from cannot be in the past'", status, out)
	}
	afterLists, _ := ptCountListsAndItems(t, env)
	if afterLists != beforeLists {
		t.Fatalf("past effective_from wrote price lists: %d→%d", beforeLists, afterLists)
	}
}

// TestPriceTemplate_ApplyConflicts — an existing price_list code and a
// conflicting merchant item type both 409.
func TestPriceTemplate_ApplyConflicts(t *testing.T) {
	env := plNewEnv(t, "tplconflict")
	templateID, _ := ptTemplate(t, env)

	// An existing list with the code the apply wants to use.
	existingCode := "bpjs-taken-" + ptSeq()
	plCreateList(t, env, existingCode, "", "", "")

	status, out := ptApplyWithCode(t, env, templateID, "bidan_jejaring", existingCode, map[string]string{})
	if status != http.StatusConflict || plErrorMsg(out) != "price list code already exists" {
		t.Fatalf("taken code = %d %v, want 409 'price list code already exists'", status, out)
	}

	// A merchant item ANC-BIDAN of type consultation blocks the apply.
	plCreateItem(t, env, "ANC-BIDAN", "consultation")
	status, out = ptApply(t, env, templateID, "bidan_jejaring", map[string]string{})
	if status != http.StatusConflict || !strings.Contains(plErrorMsg(out), "ANC-BIDAN") {
		t.Fatalf("type conflict = %d %v, want 409 mentioning ANC-BIDAN", status, out)
	}
	if !strings.Contains(plErrorMsg(out), "consultation") {
		t.Fatalf("type conflict error = %q, want mentions consultation", plErrorMsg(out))
	}
}

// ptApplyWithCode applies with an explicit price_list_code.
func ptApplyWithCode(t *testing.T, env *qeEnv, templateID, scope, code string, values map[string]string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{
		"merchant_id":     env.merchantID,
		"price_list_code": code,
		"price_list_name": "BPJS " + scope,
		"scope":           scope,
		"effective_from":  plToday(env, 0),
		"values":          values,
	}
	return qeRequest(t, env, http.MethodPost, "/api/v1/price-list-templates/"+templateID+"/apply", body)
}

// TestPriceTemplate_Reuse — an existing merchant procedure with the same code
// (KB-SUNTIK) is reused, not duplicated.
func TestPriceTemplate_Reuse(t *testing.T) {
	env := plNewEnv(t, "tplreuse")
	templateID, _ := ptTemplate(t, env)
	plCreateItem(t, env, "KB-SUNTIK", "procedure")

	status, out := ptApply(t, env, templateID, "bidan_jejaring", map[string]string{})
	if status != http.StatusCreated {
		t.Fatalf("apply = %d: %v", status, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("missing data: %v", out)
	}
	reused, _ := data["reused"].([]any)
	created, _ := data["created"].([]any)
	var foundReused, foundCreated bool
	for _, v := range reused {
		if v.(string) == "KB-SUNTIK" {
			foundReused = true
		}
	}
	for _, v := range created {
		if v.(string) == "KB-SUNTIK" {
			foundCreated = true
		}
	}
	if !foundReused || foundCreated {
		t.Fatalf("KB-SUNTIK reused=%v created=%v (want reused, not created): %v", foundReused, foundCreated, data)
	}
	if n := ptItemCount(t, env, "KB-SUNTIK"); n != 1 {
		t.Fatalf("KB-SUNTIK rows = %d, want 1", n)
	}
}
