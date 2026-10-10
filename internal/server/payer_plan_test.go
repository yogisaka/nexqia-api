//go:build integration

package server_test

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/yogisaka/nexqia-api/internal/auth"
	"github.com/yogisaka/nexqia-api/internal/server"
)

// TestPayer_* covers the payer API (spec 2026-10-07-tariff-b1-billing-core
// §6): payer CRUD with price list + BPJS uniqueness + active-visit guard,
// service rules (blacklist / discount), plans with care rules and inner
// limits, BPJS item coverage with the non-capitation template import, the
// facility/billing profile and the core.tariff.manage permission.

// payNewEnv is dnNewEnv (Jakarta) with a payer-test label.
func payNewEnv(t *testing.T, label string) *qeEnv {
	t.Helper()
	return dnNewEnv(t, "pay."+label, "Asia/Jakarta")
}

// payCreatePayer creates a payer and asserts the given status.
func payCreatePayer(t *testing.T, env *qeEnv, code, name, payerType, priceListID string) (int, map[string]any) {
	t.Helper()
	body := map[string]any{"code": code, "name": name, "payer_type": payerType}
	if priceListID != "" {
		body["price_list_id"] = priceListID
	}
	return qeRequest(t, env, http.MethodPost, "/api/v1/merchants/"+env.merchantID+"/payers", body)
}

// payMapData asserts status and returns the data object.
func payMapData(t *testing.T, status int, out map[string]any, want int) map[string]any {
	t.Helper()
	if status != want {
		t.Fatalf("status = %d, want %d: %v", status, want, out)
	}
	data, _ := out["data"].(map[string]any)
	if data == nil {
		t.Fatalf("missing data object: %v", out)
	}
	return data
}

// payDataSlice returns data as a slice of objects.
func payDataSlice(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	arr, _ := out["data"].([]any)
	res := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		m, _ := v.(map[string]any)
		res = append(res, m)
	}
	return res
}

// payRules returns data.rules as a slice of objects.
func payRules(t *testing.T, out map[string]any) []map[string]any {
	t.Helper()
	data, _ := out["data"].(map[string]any)
	arr, _ := data["rules"].([]any)
	res := make([]map[string]any, 0, len(arr))
	for _, v := range arr {
		m, _ := v.(map[string]any)
		res = append(res, m)
	}
	return res
}

// TestPayer_CRUD — create with/without price list, unknown price list, code
// conflict, patch with optimistic locking, PascalCase list exposure, and the
// soft-delete guard on an active visit.
func TestPayer_CRUD(t *testing.T) {
	env := payNewEnv(t, "crud")
	ctx := context.Background()

	listID := plCreateList(t, env, "pay-base", "", "", "")
	status, out := payCreatePayer(t, env, "PRV-A", "Asuransi A", "private_insurance", listID)
	created := payMapData(t, status, out, http.StatusCreated)
	if created["payer_type"] != "private_insurance" || created["price_list_id"] != listID {
		t.Fatalf("unexpected create payload: %v", created)
	}
	if created["is_active"] != true || created["row_version"] != float64(1) {
		t.Fatalf("unexpected defaults: %v", created)
	}
	payerID, _ := created["id"].(string)

	// Second payer without price list → null price_list_id.
	status, out = payCreatePayer(t, env, "PRV-B", "Asuransi B", "company_guarantor", "")
	noList := payMapData(t, status, out, http.StatusCreated)
	if noList["price_list_id"] != nil {
		t.Fatalf("price_list_id = %v, want nil", noList["price_list_id"])
	}

	// Unknown price list → 404.
	status, out = payCreatePayer(t, env, "PRV-C", "Asuransi C", "private_insurance", "11111111-1111-1111-1111-111111111111")
	if status != http.StatusNotFound || plErrorMsg(out) != "price list not found" {
		t.Fatalf("unknown price list = %d %v", status, out)
	}

	// Duplicate code → 409.
	status, out = payCreatePayer(t, env, "PRV-A", "Asuransi A lagi", "private_insurance", "")
	if status != http.StatusConflict || plErrorMsg(out) != "payer code already exists" {
		t.Fatalf("duplicate code = %d %v", status, out)
	}

	// Patch with a stale row_version → 409.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/payers/"+payerID, map[string]any{
		"name": "Asuransi A Baru", "is_active": false, "row_version": 99,
	})
	if status != http.StatusConflict {
		t.Fatalf("stale row_version = %d %v", status, out)
	}

	// Patch ok.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/payers/"+payerID, map[string]any{
		"name": "Asuransi A Baru", "is_active": false, "row_version": 1,
	})
	updated := payMapData(t, status, out, http.StatusOK)
	if updated["name"] != "Asuransi A Baru" || updated["is_active"] != false || updated["row_version"] != float64(2) {
		t.Fatalf("unexpected patch result: %v", updated)
	}

	// Existing list endpoint stays PascalCase and exposes the new column.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/payers", nil)
	if status != http.StatusOK {
		t.Fatalf("payer list = %d %v", status, out)
	}
	rows := payDataSlice(t, out)
	if len(rows) == 0 {
		t.Fatalf("empty payer list: %v", out)
	}
	found := false
	for _, row := range rows {
		if row["ID"] == payerID {
			found = true
			if _, ok := row["PriceListID"]; !ok {
				t.Fatalf("PriceListID missing from list row: %v", row)
			}
		}
	}
	if !found {
		t.Fatalf("payer %s missing from PascalCase list: %v", payerID, rows)
	}

	// Active-visit guard: a registered visit guarantees with this payer.
	personID, _ := mergeCreatePerson(t, ctx, env.pool, env.router, env.token, env.companyID, env.merchantID,
		map[string]string{"full_name": "Payee One", "gender": "male", "birth_date": "1990-01-01"})
	mergeSeedAdmission(t, ctx, env.pool, env.companyID, env.merchantID, personID, "paycrud")
	// mergeSeedAdmission seeds its own admission; find it via the visit label.
	var admissionID string
	if err := env.pool.QueryRow(ctx,
		"SELECT id::text FROM operations.admission WHERE merchant_id = $1 AND person_id = $2 ORDER BY created_at DESC LIMIT 1",
		env.merchantID, personID).Scan(&admissionID); err != nil {
		t.Fatalf("find admission: %v", err)
	}
	if _, err := env.pool.Exec(ctx,
		"INSERT INTO operations.admission_guarantor (admission_id, payer_id) VALUES ($1, $2)",
		admissionID, payerID); err != nil {
		t.Fatalf("seed guarantor: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodDelete, "/api/v1/payers/"+payerID, nil)
	if status != http.StatusConflict || plErrorMsg(out) != "payer is used by an active visit" {
		t.Fatalf("delete in-use payer = %d %v", status, out)
	}
	// Discharge the visit → delete succeeds.
	if _, err := env.pool.Exec(ctx,
		"UPDATE operations.admission SET status = 'discharged' WHERE id = $1", admissionID); err != nil {
		t.Fatalf("discharge: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodDelete, "/api/v1/payers/"+payerID, nil)
	deleted := payMapData(t, status, out, http.StatusOK)
	if deleted["deleted"] != true {
		t.Fatalf("unexpected delete payload: %v", deleted)
	}
	// Deleted payer disappears from the live list.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID+"/payers", nil)
	if status != http.StatusOK {
		t.Fatalf("payer list after delete = %d %v", status, out)
	}
	for _, row := range payDataSlice(t, out) {
		if row["ID"] == payerID {
			t.Fatalf("deleted payer still listed: %v", row)
		}
	}
}

// TestPayer_SecondBPJSRejected — only one live BPJS payer per merchant.
func TestPayer_SecondBPJSRejected(t *testing.T) {
	env := payNewEnv(t, "bpjs")
	status, out := payCreatePayer(t, env, "BPJS-1", "BPJS Induk", "bpjs", "")
	payMapData(t, status, out, http.StatusCreated)

	status, out = payCreatePayer(t, env, "BPJS-2", "BPJS Kedua", "bpjs", "")
	if status != http.StatusConflict || plErrorMsg(out) != "merchant already has a BPJS payer" {
		t.Fatalf("second bpjs = %d %v", status, out)
	}

	// Another payer type is still fine.
	status, out = payCreatePayer(t, env, "PRV-X", "Asuransi X", "private_insurance", "")
	payMapData(t, status, out, http.StatusCreated)

	// Soft-delete the BPJS payer → a new BPJS payer is allowed again.
	var bpjsID string
	if err := env.pool.QueryRow(context.Background(), "SELECT id::text FROM core.payer WHERE merchant_id = $1 AND code = 'BPJS-1'", env.merchantID).Scan(&bpjsID); err != nil {
		t.Fatalf("find bpjs payer: %v", err)
	}
	status, out = qeRequest(t, env, http.MethodDelete, "/api/v1/payers/"+bpjsID, nil)
	payMapData(t, status, out, http.StatusOK)
	status, out = payCreatePayer(t, env, "BPJS-3", "BPJS Pengganti", "bpjs", "")
	payMapData(t, status, out, http.StatusCreated)
}

// TestPayer_ServiceRules — full-set replace of blacklist/discount rules with
// the exact-one-of validation.
func TestPayer_ServiceRules(t *testing.T) {
	env := payNewEnv(t, "rules")
	_, out := payCreatePayer(t, env, "PRV-R", "Asuransi R", "private_insurance", "")
	payerID, _ := out["data"].(map[string]any)["id"].(string)
	itemA := plCreateItem(t, env, "RULE-A", "procedure")
	itemB := plCreateItem(t, env, "RULE-B", "drug")

	put := func(rules []map[string]any) (int, map[string]any) {
		return qeRequest(t, env, http.MethodPut, "/api/v1/payers/"+payerID+"/service-rules", map[string]any{"rules": rules})
	}

	status, out := put([]map[string]any{
		{"service_item_id": itemA, "rule_type": "discount", "discount_percent": "10.00"},
		{"service_item_id": itemB, "rule_type": "blacklist"},
	})
	if status != http.StatusOK {
		t.Fatalf("put rules = %d %v", status, out)
	}
	rules := payRules(t, out)
	if len(rules) != 2 {
		t.Fatalf("rules = %v", rules)
	}
	if rules[0]["rule_type"] != "discount" || rules[0]["discount_percent"] != "10.00" || rules[0]["discount_amount"] != nil {
		t.Fatalf("discount rule = %v", rules[0])
	}

	// Discount with both values / neither / bad percent / zero amount → 400.
	bad := [][]map[string]any{
		{{"service_item_id": itemA, "rule_type": "discount", "discount_percent": "10.00", "discount_amount": "5000.00"}},
		{{"service_item_id": itemA, "rule_type": "discount"}},
		{{"service_item_id": itemA, "rule_type": "discount", "discount_percent": "150.00"}},
		{{"service_item_id": itemA, "rule_type": "discount", "discount_amount": "0.00"}},
	}
	for i, body := range bad {
		status, out := put(body)
		if status != http.StatusBadRequest || plErrorMsg(out) != "discount needs a percent or an amount" {
			t.Fatalf("bad discount #%d = %d %v", i, status, out)
		}
	}

	// Blacklist with a value → 400.
	status, out = put([]map[string]any{{"service_item_id": itemA, "rule_type": "blacklist", "discount_percent": "5.00"}})
	if status != http.StatusBadRequest {
		t.Fatalf("blacklist with value = %d %v", status, out)
	}

	// Unknown item → 404.
	status, out = put([]map[string]any{{"service_item_id": "22222222-2222-2222-2222-222222222222", "rule_type": "blacklist"}})
	if status != http.StatusNotFound || plErrorMsg(out) != "service item not found" {
		t.Fatalf("unknown item = %d %v", status, out)
	}

	// Full-set replace: only the blacklist survives.
	status, out = put([]map[string]any{{"service_item_id": itemB, "rule_type": "blacklist"}})
	if status != http.StatusOK {
		t.Fatalf("replace = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+payerID+"/service-rules", nil)
	payMapData(t, status, out, http.StatusOK)
	rules = payRules(t, out)
	if len(rules) != 1 || rules[0]["rule_type"] != "blacklist" {
		t.Fatalf("rules after replace = %v", rules)
	}
}

// TestPayer_PlanRulesLimits — plan CRUD, bpjs/self_pay rejection, care rules
// and inner limits with full-set replace.
func TestPayer_PlanRulesLimits(t *testing.T) {
	env := payNewEnv(t, "plans")

	// self_pay payer cannot own plans.
	_, out := payCreatePayer(t, env, "SELF-1", "Pasien Umum", "self_pay", "")
	selfID, _ := out["data"].(map[string]any)["id"].(string)
	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+selfID+"/plans", nil)
	if status != http.StatusConflict || plErrorMsg(out) != "this payer type has no plans" {
		t.Fatalf("self_pay plans = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+selfID+"/plans",
		map[string]any{"code": "PL-S", "name": "Plan S"})
	if status != http.StatusConflict || plErrorMsg(out) != "this payer type has no plans" {
		t.Fatalf("self_pay create plan = %d %v", status, out)
	}

	// Private payer: create + list.
	_, out = payCreatePayer(t, env, "PRV-P", "Asuransi P", "private_insurance", "")
	payerID, _ := out["data"].(map[string]any)["id"].(string)
	planBody := map[string]any{
		"code": "PL-1", "name": "Plan Satu", "requires_gl": true, "annual_limit": "1000000.00",
	}
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+payerID+"/plans", planBody)
	plan := payMapData(t, status, out, http.StatusCreated)
	if plan["annual_limit"] != "1000000.00" || plan["requires_gl"] != true || plan["row_version"] != float64(1) {
		t.Fatalf("unexpected plan payload: %v", plan)
	}
	if r, _ := plan["rules"].([]any); len(r) != 0 {
		t.Fatalf("new plan has rules: %v", plan)
	}
	if l, _ := plan["limits"].([]any); len(l) != 0 {
		t.Fatalf("new plan has limits: %v", plan)
	}
	planID, _ := plan["id"].(string)

	// Duplicate plan code → 409.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+payerID+"/plans", planBody)
	if status != http.StatusConflict || plErrorMsg(out) != "plan code already exists" {
		t.Fatalf("duplicate plan code = %d %v", status, out)
	}

	// Care rules (full set).
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payer-plans/"+planID+"/rules", map[string]any{
		"rules": []map[string]any{{
			"care_type": "outpatient", "copay_percent": "10.00",
			"copay_max_per_claim": "50000.00", "deductible_per_claim": "0.00", "visit_limit": "4.00",
		}},
	})
	plan = payMapData(t, status, out, http.StatusOK)
	rules, _ := plan["rules"].([]any)
	if len(rules) != 1 {
		t.Fatalf("plan rules = %v", plan)
	}
	rule0, _ := rules[0].(map[string]any)
	if rule0["care_type"] != "outpatient" || rule0["copay_percent"] != "10.00" ||
		rule0["copay_max_per_claim"] != "50000.00" || rule0["visit_limit"] != "4.00" {
		t.Fatalf("rule row = %v", rule0)
	}

	// Inner limits (full set).
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payer-plans/"+planID+"/limits", map[string]any{
		"limits": []map[string]any{
			{"item_type": "consultation", "limit_per_visit": "150000.00"},
			{"item_type": "drug", "limit_per_visit": "300000.00"},
		},
	})
	plan = payMapData(t, status, out, http.StatusOK)
	limits, _ := plan["limits"].([]any)
	if len(limits) != 2 {
		t.Fatalf("plan limits = %v", plan)
	}

	// GET /plans returns plan + rules + limits.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+payerID+"/plans", nil)
	if status != http.StatusOK {
		t.Fatalf("plans list = %d %v", status, out)
	}
	list := payDataSlice(t, out)
	if len(list) != 1 {
		t.Fatalf("plans list = %v", out)
	}
	if r, _ := list[0]["rules"].([]any); len(r) != 1 {
		t.Fatalf("listed plan rules = %v", list[0])
	}
	if l, _ := list[0]["limits"].([]any); len(l) != 2 {
		t.Fatalf("listed plan limits = %v", list[0])
	}

	// Patch with optimistic locking: stale then fresh.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/payer-plans/"+planID, map[string]any{
		"name": "Plan Satu Baru", "row_version": 42,
	})
	if status != http.StatusConflict {
		t.Fatalf("stale plan patch = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/payer-plans/"+planID, map[string]any{
		"name": "Plan Satu Baru", "row_version": 1,
	})
	plan = payMapData(t, status, out, http.StatusOK)
	if plan["name"] != "Plan Satu Baru" || plan["row_version"] != float64(2) {
		t.Fatalf("patched plan = %v", plan)
	}

	// Delete → 200, list empty.
	status, out = qeRequest(t, env, http.MethodDelete, "/api/v1/payer-plans/"+planID, nil)
	payMapData(t, status, out, http.StatusOK)
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+payerID+"/plans", nil)
	if status != http.StatusOK {
		t.Fatalf("plans list after delete = %d %v", status, out)
	}
	if got := payDataSlice(t, out); len(got) != 0 {
		t.Fatalf("plans after delete = %v", out)
	}

	// bpjs payer cannot own plans either.
	_, out = payCreatePayer(t, env, "BPJS-PL", "BPJS Plan", "bpjs", "")
	bpjsID, _ := out["data"].(map[string]any)["id"].(string)
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+bpjsID+"/plans",
		map[string]any{"code": "PL-B", "name": "Plan B"})
	if status != http.StatusConflict || plErrorMsg(out) != "this payer type has no plans" {
		t.Fatalf("bpjs create plan = %d %v", status, out)
	}
}

// TestPayer_ItemCoverageFromTemplate — full-set replace of BPJS item coverage
// and the non-capitation template import (prefix → claim type, first match
// wins; already-covered items skipped).
func TestPayer_ItemCoverageFromTemplate(t *testing.T) {
	env := payNewEnv(t, "cov")

	// Non-BPJS payer → 409 on every coverage endpoint.
	_, out := payCreatePayer(t, env, "PRV-K", "Asuransi K", "private_insurance", "")
	privID, _ := out["data"].(map[string]any)["id"].(string)
	status, out := qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+privID+"/item-coverage", nil)
	if status != http.StatusConflict || plErrorMsg(out) != "item coverage applies to the BPJS payer only" {
		t.Fatalf("private coverage get = %d %v", status, out)
	}

	// BPJS payer with template-matching items.
	_, out = payCreatePayer(t, env, "BPJS-C", "BPJS Cov", "bpjs", "")
	bpjsID, _ := out["data"].(map[string]any)["id"].(string)
	ancItem := plCreateItem(t, env, "ANC-BIDAN", "procedure")
	skrItem := plCreateItem(t, env, "SKR-IVA", "procedure")
	krioItem := plCreateItem(t, env, "KRIO", "procedure")

	// Manual coverage first: one item excluded (will be skipped by import),
	// one item_type rule.
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payers/"+bpjsID+"/item-coverage", map[string]any{
		"rules": []map[string]any{
			{"service_item_id": ancItem, "coverage": "excluded"},
			{"item_type": "drug", "coverage": "capitation"},
		},
	})
	if status != http.StatusOK {
		t.Fatalf("put coverage = %d %v", status, out)
	}
	rules := payRules(t, out)
	if len(rules) != 2 {
		t.Fatalf("coverage rules = %v", rules)
	}

	// Validation errors.
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payers/"+bpjsID+"/item-coverage", map[string]any{
		"rules": []map[string]any{{"coverage": "claim"}},
	})
	if status != http.StatusBadRequest || plErrorMsg(out) != "exactly one of service_item_id or item_type is required" {
		t.Fatalf("both targets = %d %v", status, out)
	}
	status, out = qeRequest(t, env, http.MethodPut, "/api/v1/payers/"+bpjsID+"/item-coverage", map[string]any{
		"rules": []map[string]any{{"service_item_id": ancItem, "coverage": "claim"}},
	})
	if status != http.StatusBadRequest || plErrorMsg(out) != "claim_type is required for claim coverage" {
		t.Fatalf("claim without type = %d %v", status, out)
	}

	// Import: ANC-BIDAN already has a rule (skipped); SKR-IVA → screening, KRIO → other.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+bpjsID+"/item-coverage/from-template", nil)
	imp := payMapData(t, status, out, http.StatusOK)
	if imp["created"] != float64(2) || imp["skipped"] != float64(1) {
		t.Fatalf("import = %v", imp)
	}
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/payers/"+bpjsID+"/item-coverage", nil)
	payMapData(t, status, out, http.StatusOK)
	byItem := map[string]map[string]any{}
	for _, r := range payRules(t, out) {
		if id, _ := r["service_item_id"].(string); id != "" {
			byItem[id] = r
		}
	}
	if r := byItem[skrItem]; r == nil || r["coverage"] != "claim" || r["claim_type"] != "screening" {
		t.Fatalf("SKR-IVA rule = %v", r)
	}
	if r := byItem[krioItem]; r == nil || r["coverage"] != "claim" || r["claim_type"] != "other" {
		t.Fatalf("KRIO rule = %v", r)
	}
	if r := byItem[ancItem]; r == nil || r["coverage"] != "excluded" {
		t.Fatalf("ANC-BIDAN rule = %v", r)
	}

	// Re-import: everything already covered.
	status, out = qeRequest(t, env, http.MethodPost, "/api/v1/payers/"+bpjsID+"/item-coverage/from-template", nil)
	imp = payMapData(t, status, out, http.StatusOK)
	if imp["created"] != float64(0) || imp["skipped"] != float64(3) {
		t.Fatalf("re-import = %v", imp)
	}
}

// TestPayer_BillingProfile — facility/BPJS profile patch, the FKTP-network
// parent constraint, and the PascalCase GET /merchants/:id exposure.
func TestPayer_BillingProfile(t *testing.T) {
	env := payNewEnv(t, "profile")
	listID := plCreateList(t, env, "prof-dpl", "", "", "")

	// Partial patch: facility type only.
	status, out := qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"facility_type": "klinik_pratama"})
	profile := payMapData(t, status, out, http.StatusOK)
	if profile["facility_type"] != "klinik_pratama" || profile["bpjs_cooperation"] != "none" {
		t.Fatalf("profile after facility patch = %v", profile)
	}

	// fktp_network without parent → 400 with the exact contract message.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"bpjs_cooperation": "fktp_network"})
	if status != http.StatusBadRequest || plErrorMsg(out) != "network FKTP needs the parent FKTP" {
		t.Fatalf("network without parent = %d %v", status, out)
	}

	// With parent + default price list → 200.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{
			"bpjs_cooperation": "fktp_network", "bpjs_network_parent": "Puskesmas Sukamaju",
			"default_price_list_id": listID,
		})
	profile = payMapData(t, status, out, http.StatusOK)
	if profile["bpjs_cooperation"] != "fktp_network" || profile["bpjs_network_parent"] != "Puskesmas Sukamaju" ||
		profile["default_price_list_id"] != listID {
		t.Fatalf("profile after network patch = %v", profile)
	}

	// Unknown price list → 404.
	status, out = qeRequest(t, env, http.MethodPatch, "/api/v1/merchants/"+env.merchantID+"/billing-profile",
		map[string]any{"default_price_list_id": "33333333-3333-3333-3333-333333333333"})
	if status != http.StatusNotFound || plErrorMsg(out) != "price list not found" {
		t.Fatalf("unknown default list = %d %v", status, out)
	}

	// GET /merchants/:id (existing, PascalCase) carries the four fields.
	status, out = qeRequest(t, env, http.MethodGet, "/api/v1/merchants/"+env.merchantID, nil)
	merchant := payMapData(t, status, out, http.StatusOK)
	if merchant["FacilityType"] != "klinik_pratama" || merchant["BpjsCooperation"] != "fktp_network" {
		t.Fatalf("merchant payload = %v", merchant)
	}
	if merchant["BpjsNetworkParent"] != "Puskesmas Sukamaju" || merchant["DefaultPriceListID"] != listID {
		t.Fatalf("merchant payload = %v", merchant)
	}
}

// TestPayer_RequiresPermission — a user without core.tariff.manage gets 403.
func TestPayer_RequiresPermission(t *testing.T) {
	ctx := context.Background()
	pool := newTestPostgresPool(t, ctx)
	redisClient := newTestRedisClient(t, ctx)
	router := server.NewRouter(pool, redisClient, testConfig())
	companyID, merchantID, _, _, _ := seedAccountOwner(t, ctx, pool, router, "pay.perm", "081234571201")

	restrictedUserID := "7e7e7e7e-7e7e-7e7e-7e7e-7e7e7e7e7e7e"
	roleID := "7f7f7f7f-7f7f-7f7f-7f7f-7f7f7f7f7f7f"
	if _, err := pool.Exec(ctx, "INSERT INTO core.role (id, company_id, name) VALUES ($1, $2, 'No Payer')", roleID, companyID); err != nil {
		t.Fatalf("seed role: %v", err)
	}
	passwordHash, err := testHasher().Hash(ctx, "correct-horse")
	if err != nil {
		t.Fatalf("hash password: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.app_user (id, company_id, username, password_hash, is_active) VALUES ($1, $2, 'pay.noperm', $3, true)", restrictedUserID, companyID, passwordHash); err != nil {
		t.Fatalf("seed user: %v", err)
	}
	if _, err := pool.Exec(ctx, "INSERT INTO core.user_merchant_role (user_id, merchant_id, role_id) VALUES ($1, $2, $3)", restrictedUserID, merchantID, roleID); err != nil {
		t.Fatalf("seed user_merchant_role: %v", err)
	}
	restrictedToken, err := auth.GenerateToken(testJWTSecret, restrictedUserID, companyID, merchantID, "", "pay.noperm", "pay-noperm-device", time.Hour)
	if err != nil {
		t.Fatalf("generate token: %v", err)
	}

	status, out := qeRequestToken(t, router, http.MethodPost, "/api/v1/merchants/"+merchantID+"/payers", restrictedToken, companyID, merchantID,
		map[string]any{"code": "NP-1", "name": "No Perm", "payer_type": "private_insurance"})
	if status != http.StatusForbidden {
		t.Fatalf("POST payer without permission = %d %v", status, out)
	}
	status, out = qeRequestToken(t, router, http.MethodPatch, "/api/v1/merchants/"+merchantID+"/billing-profile", restrictedToken, companyID, merchantID,
		map[string]any{"facility_type": "klinik_pratama"})
	if status != http.StatusForbidden {
		t.Fatalf("PATCH billing profile without permission = %d %v", status, out)
	}
	status, out = qeRequestToken(t, router, http.MethodGet, "/api/v1/merchants/"+merchantID+"/payers", restrictedToken, companyID, merchantID, nil)
	if status != http.StatusForbidden {
		t.Fatalf("GET payers without permission = %d %v", status, out)
	}
}
